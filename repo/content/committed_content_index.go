package content

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/gather"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content/index"
	"github.com/kopia/kopia/repo/logging"
)

// smallIndexEntryCountThreshold is the threshold to determine whether an
// index is small. Any index with fewer entries than this threshold
// will be combined in-memory to reduce the number of segments and speed up
// large index operations (such as verification of all contents).
const smallIndexEntryCountThreshold = 100

type committedContentIndex struct {
	// +checkatomic
	rev   int64
	cache committedContentIndexCache

	mu sync.Mutex
	// +checklocks:mu
	deletionWatermark time.Time
	// +checklocks:mu
	inUse map[blob.ID]index.Index
	// +checklocks:mu
	merged index.Merged

	v1PerContentOverhead uint32
	indexVersion         int

	// fetchOne loads one index blob
	fetchOne func(ctx context.Context, blobID blob.ID, output *gather.WriteBuffer) error

	log logging.Logger
}

type committedContentIndexCache interface {
	hasIndexBlobID(ctx context.Context, indexBlob blob.ID) (bool, error)
	addContentToCache(ctx context.Context, indexBlob blob.ID, data gather.Bytes) error
	openIndex(ctx context.Context, indexBlob blob.ID) (index.Index, error)
	expireUnused(ctx context.Context, used []blob.ID) error
}

func (c *committedContentIndex) revision() int64 {
	return atomic.LoadInt64(&c.rev)
}

func (c *committedContentIndex) getContent(contentID ID) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	info, err := c.merged.GetInfo(contentID)
	if info != nil {
		if shouldIgnore(info, c.deletionWatermark) {
			return nil, ErrContentNotFound
		}

		return info, nil
	}

	if err == nil {
		return nil, ErrContentNotFound
	}

	return nil, errors.Wrap(err, "error getting content info from index")
}

func shouldIgnore(id Info, deletionWatermark time.Time) bool {
	if !id.GetDeleted() {
		return false
	}

	return !id.Timestamp().After(deletionWatermark)
}

func (c *committedContentIndex) addIndexBlob(ctx context.Context, indexBlobID blob.ID, data gather.Bytes, use bool) error {
	// ensure we bump revision number AFTER this function
	// doing it prematurely might confuse callers of revision() who may cache
	// a set of old contents and associate it with new revision, before new contents
	// are actually available.
	defer func() {
		atomic.AddInt64(&c.rev, 1)
	}()

	if err := c.cache.addContentToCache(ctx, indexBlobID, data); err != nil {
		return errors.Wrap(err, "error adding content to cache")
	}

	if !use {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inUse[indexBlobID] != nil {
		return nil
	}

	c.log.Debugf("use-new-committed-index %v", indexBlobID)

	ndx, err := c.cache.openIndex(ctx, indexBlobID)
	if err != nil {
		return errors.Wrapf(err, "unable to open pack index %q", indexBlobID)
	}

	c.inUse[indexBlobID] = ndx
	c.merged = append(c.merged, ndx)

	return nil
}

func (c *committedContentIndex) listContents(r IDRange, cb func(i Info) error) error {
	c.mu.Lock()
	m := append(index.Merged(nil), c.merged...)
	deletionWatermark := c.deletionWatermark
	c.mu.Unlock()

	// nolint:wrapcheck
	return m.Iterate(r, func(i Info) error {
		if shouldIgnore(i, deletionWatermark) {
			return nil
		}

		return cb(i)
	})
}

// +checklocks:c.mu
func (c *committedContentIndex) indexFilesChanged(indexFiles []blob.ID) bool {
	if len(indexFiles) != len(c.inUse) {
		return true
	}

	for _, ndx := range indexFiles {
		if c.inUse[ndx] == nil {
			return true
		}
	}

	return false
}

func (c *committedContentIndex) merge(ctx context.Context, indexFiles []blob.ID) (merged index.Merged, used map[blob.ID]index.Index, finalErr error) {
	used = map[blob.ID]index.Index{}

	defer func() {
		// we failed along the way, close the merged index.
		if finalErr != nil {
			merged.Close() //nolint:errcheck
		}
	}()

	for _, e := range indexFiles {
		ndx, err := c.cache.openIndex(ctx, e)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "unable to open pack index %q", e)
		}

		merged = append(merged, ndx)
		used[e] = ndx
	}

	mergedAndCombined, err := c.combineSmallIndexes(merged)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to combine small indexes")
	}

	c.log.Debugf("combined %v into %v index segments", len(merged), len(mergedAndCombined))

	merged = mergedAndCombined

	return
}

// Uses indexFiles for indexing. An error is returned if the
// indices cannot be read for any reason.
func (c *committedContentIndex) use(ctx context.Context, indexFiles []blob.ID, ignoreDeletedBefore time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.deletionWatermark = ignoreDeletedBefore

	if !c.indexFilesChanged(indexFiles) {
		return nil
	}

	c.log.Debugf("use-indexes %v", indexFiles)

	mergedAndCombined, newInUse, err := c.merge(ctx, indexFiles)
	if err != nil {
		return err
	}

	atomic.AddInt64(&c.rev, 1)

	c.merged = mergedAndCombined
	c.inUse = newInUse

	if err := c.cache.expireUnused(ctx, indexFiles); err != nil {
		c.log.Errorf("unable to expire unused index files: %v", err)
	}

	return nil
}

func (c *committedContentIndex) combineSmallIndexes(m index.Merged) (index.Merged, error) {
	var toKeep, toMerge index.Merged

	for _, ndx := range m {
		if ndx.ApproximateCount() < smallIndexEntryCountThreshold {
			toMerge = append(toMerge, ndx)
		} else {
			toKeep = append(toKeep, ndx)
		}
	}

	if len(toMerge) <= 1 {
		return m, nil
	}

	b := index.Builder{}

	for _, ndx := range toMerge {
		if err := ndx.Iterate(index.AllIDs, func(i Info) error {
			b.Add(i)
			return nil
		}); err != nil {
			return nil, errors.Wrap(err, "unable to iterate index entries")
		}
	}

	var buf bytes.Buffer

	if err := b.Build(&buf, c.indexVersion); err != nil {
		return nil, errors.Wrap(err, "error building combined in-memory index")
	}

	combined, err := index.Open(bytes.NewReader(buf.Bytes()), c.v1PerContentOverhead)
	if err != nil {
		return nil, errors.Wrap(err, "error opening combined in-memory index")
	}

	return append(toKeep, combined), nil
}

func (c *committedContentIndex) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pi := range c.inUse {
		if err := pi.Close(); err != nil {
			return errors.Wrap(err, "unable to close index")
		}
	}

	return nil
}

func (c *committedContentIndex) fetchIndexBlobs(ctx context.Context, indexBlobs []blob.ID) error {
	ch, err := c.missingIndexBlobs(ctx, indexBlobs)
	if err != nil {
		return err
	}

	if len(ch) == 0 {
		return nil
	}

	c.log.Debugf("Downloading %v new index blobs...", len(indexBlobs))

	eg, ctx := errgroup.WithContext(ctx)
	for i := 0; i < parallelFetches; i++ {
		eg.Go(func() error {
			var data gather.WriteBuffer
			defer data.Close()

			for indexBlobID := range ch {
				data.Reset()

				if err := c.fetchOne(ctx, indexBlobID, &data); err != nil {
					return errors.Wrapf(err, "error loading index blob %v", indexBlobID)
				}

				if err := c.addIndexBlob(ctx, indexBlobID, data.Bytes(), false); err != nil {
					return errors.Wrap(err, "unable to add to committed content cache")
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return errors.Wrap(err, "error downloading indexes")
	}

	c.log.Debugf("Index blobs downloaded.")

	return nil
}

// missingIndexBlobs returns a closed channel filled with blob IDs that are not in committedContents cache.
func (c *committedContentIndex) missingIndexBlobs(ctx context.Context, blobs []blob.ID) (<-chan blob.ID, error) {
	ch := make(chan blob.ID, len(blobs))
	defer close(ch)

	for _, id := range blobs {
		has, err := c.cache.hasIndexBlobID(ctx, id)
		if err != nil {
			return nil, errors.Wrapf(err, "error determining whether index blob %v has been downloaded", id)
		}

		if !has {
			ch <- id
		}
	}

	return ch, nil
}

func newCommittedContentIndex(caching *CachingOptions,
	v1PerContentOverhead uint32,
	indexVersion int,
	fetchOne func(ctx context.Context, blobID blob.ID, output *gather.WriteBuffer) error,
	log logging.Logger,
	minSweepAge time.Duration,
) *committedContentIndex {
	var cache committedContentIndexCache

	if caching.CacheDirectory != "" {
		dirname := filepath.Join(caching.CacheDirectory, "indexes")
		cache = &diskCommittedContentIndexCache{dirname, clock.Now, v1PerContentOverhead, log, minSweepAge}
	} else {
		cache = &memoryCommittedContentIndexCache{
			contents:             map[blob.ID]index.Index{},
			v1PerContentOverhead: v1PerContentOverhead,
		}
	}

	return &committedContentIndex{
		cache:                cache,
		inUse:                map[blob.ID]index.Index{},
		v1PerContentOverhead: v1PerContentOverhead,
		indexVersion:         indexVersion,
		fetchOne:             fetchOne,
		log:                  log,
	}
}
