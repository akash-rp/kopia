package snapshotfs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/ignorefs"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/iocopy"
	"github.com/kopia/kopia/internal/timetrack"
	"github.com/kopia/kopia/internal/workshare"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/logging"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
)

// DefaultCheckpointInterval is the default frequency of mid-upload checkpointing.
const DefaultCheckpointInterval = 45 * time.Minute

var (
	uploadLog   = logging.Module("uploader")
	estimateLog = logging.Module("estimate")
	repoFSLog   = logging.Module("repofs")
)

// minimal detail levels to emit particular pieces of log information.
const (
	minDetailLevelDuration = 1
	minDetailLevelSize     = 3
	minDetailLevelDirStats = 5
	minDetailLevelModTime  = 6
	minDetailLevelOID      = 7
)

var errCanceled = errors.New("canceled")

// reasons why a snapshot is incomplete.
const (
	IncompleteReasonCheckpoint   = "checkpoint"
	IncompleteReasonCanceled     = "canceled"
	IncompleteReasonLimitReached = "limit reached"
)

// Uploader supports efficient uploading files and directories to repository.
type Uploader struct {
	// values aligned to 8-bytes due to atomic access
	// +checkatomic
	totalWrittenBytes int64

	Progress UploadProgress

	// automatically cancel the Upload after certain number of bytes
	MaxUploadBytes int64

	// probability with cached entries will be ignored, must be [0..100]
	// 0=always use cached object entries if possible
	// 100=never use cached entries
	ForceHashPercentage float64

	// Number of files to hash and upload in parallel.
	ParallelUploads int

	// Enable snapshot actions
	EnableActions bool

	// override the directory log level and entry log verbosity.
	OverrideDirLogDetail   *policy.LogDetail
	OverrideEntryLogDetail *policy.LogDetail

	// Fail the entire snapshot on source file/directory error.
	FailFast bool

	// How frequently to create checkpoint snapshot entries.
	CheckpointInterval time.Duration

	// When set to true, do not ignore any files, regardless of policy settings.
	DisableIgnoreRules bool

	repo repo.RepositoryWriter

	// stats must be allocated on heap to enforce 64-bit alignment due to atomic access on ARM.
	stats *snapshot.Stats

	// +checkatomic
	canceled int32

	getTicker func(time.Duration) <-chan time.Time

	// for testing only, when set will write to a given channel whenever checkpoint completes
	checkpointFinished chan struct{}

	// disable snapshot size estimation
	disableEstimation bool

	workerPool *workshare.Pool
}

// IsCanceled returns true if the upload is canceled.
func (u *Uploader) IsCanceled() bool {
	return u.incompleteReason() != ""
}

//
func (u *Uploader) incompleteReason() string {
	if c := atomic.LoadInt32(&u.canceled) != 0; c {
		return IncompleteReasonCanceled
	}

	wb := atomic.LoadInt64(&u.totalWrittenBytes)
	if mub := u.MaxUploadBytes; mub > 0 && wb > mub {
		return IncompleteReasonLimitReached
	}

	return ""
}

func (u *Uploader) uploadFileInternal(ctx context.Context, parentCheckpointRegistry *checkpointRegistry, relativePath string, f fs.File, pol *policy.Policy, asyncWrites int) (*snapshot.DirEntry, error) {
	u.Progress.HashingFile(relativePath)
	defer u.Progress.FinishedHashingFile(relativePath, f.Size())

	if pf, ok := f.(snapshot.HasDirEntryOrNil); ok {
		switch de, err := pf.DirEntryOrNil(ctx); {
		case err != nil:
			return nil, errors.Wrap(err, "can't read placeholder")
		case err == nil && de != nil:
			// We have read sufficient information from the shallow file's extended
			// attribute to construct DirEntry.
			_, err := u.repo.VerifyObject(ctx, de.ObjectID)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid placeholder for %q contains foreign object.ID", f.Name())
			}

			return de, nil
		}
	}

	file, err := f.Open(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to open file")
	}
	defer file.Close() //nolint:errcheck

	writer := u.repo.NewObjectWriter(ctx, object.WriterOptions{
		Description: "FILE:" + f.Name(),
		Compressor:  pol.CompressionPolicy.CompressorForFile(f),
		AsyncWrites: asyncWrites,
	})
	defer writer.Close() //nolint:errcheck

	parentCheckpointRegistry.addCheckpointCallback(f, func() (*snapshot.DirEntry, error) {
		// nolint:govet
		checkpointID, err := writer.Checkpoint()
		if err != nil {
			return nil, errors.Wrap(err, "checkpoint error")
		}

		if checkpointID == "" {
			return nil, nil
		}

		return newDirEntry(f, checkpointID)
	})

	defer parentCheckpointRegistry.removeCheckpointCallback(f)

	written, err := u.copyWithProgress(writer, file, 0, f.Size())
	if err != nil {
		return nil, err
	}

	fi2, err := file.Entry()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get file entry after copying")
	}

	r, err := writer.Result()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get result")
	}

	de, err := newDirEntry(fi2, r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create dir entry")
	}

	de.FileSize = written

	atomic.AddInt32(&u.stats.TotalFileCount, 1)
	atomic.AddInt64(&u.stats.TotalFileSize, de.FileSize)

	return de, nil
}

func (u *Uploader) uploadSymlinkInternal(ctx context.Context, relativePath string, f fs.Symlink) (*snapshot.DirEntry, error) {
	u.Progress.HashingFile(relativePath)
	defer u.Progress.FinishedHashingFile(relativePath, f.Size())

	target, err := f.Readlink(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read symlink")
	}

	writer := u.repo.NewObjectWriter(ctx, object.WriterOptions{
		Description: "SYMLINK:" + f.Name(),
	})
	defer writer.Close() //nolint:errcheck

	written, err := u.copyWithProgress(writer, bytes.NewBufferString(target), 0, f.Size())
	if err != nil {
		return nil, err
	}

	r, err := writer.Result()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get result")
	}

	de, err := newDirEntry(f, r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create dir entry")
	}

	de.FileSize = written

	return de, nil
}

func (u *Uploader) uploadStreamingFileInternal(ctx context.Context, relativePath string, f fs.StreamingFile) (*snapshot.DirEntry, error) {
	reader, err := f.GetReader(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get streaming file reader")
	}

	var streamSize int64

	u.Progress.HashingFile(relativePath)

	defer func() {
		u.Progress.FinishedHashingFile(relativePath, streamSize)
	}()

	writer := u.repo.NewObjectWriter(ctx, object.WriterOptions{
		Description: "STREAMFILE:" + f.Name(),
	})
	defer writer.Close() //nolint:errcheck

	written, err := u.copyWithProgress(writer, reader, 0, f.Size())
	if err != nil {
		return nil, err
	}

	r, err := writer.Result()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get result")
	}

	de, err := newDirEntry(f, r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create dir entry")
	}

	de.FileSize = written
	streamSize = written
	de.ModTime = clock.Now()

	atomic.AddInt32(&u.stats.TotalFileCount, 1)
	atomic.AddInt64(&u.stats.TotalFileSize, de.FileSize)

	return de, nil
}

func (u *Uploader) copyWithProgress(dst io.Writer, src io.Reader, completed, length int64) (int64, error) {
	uploadBuf := iocopy.GetBuffer()
	defer iocopy.ReleaseBuffer(uploadBuf)

	var written int64

	for {
		if u.IsCanceled() {
			return 0, errors.Wrap(errCanceled, "canceled when copying data")
		}

		readBytes, readErr := src.Read(uploadBuf)

		// nolint:nestif
		if readBytes > 0 {
			wroteBytes, writeErr := dst.Write(uploadBuf[0:readBytes])
			if wroteBytes > 0 {
				written += int64(wroteBytes)
				completed += int64(wroteBytes)
				atomic.AddInt64(&u.totalWrittenBytes, int64(wroteBytes))
				u.Progress.HashedBytes(int64(wroteBytes))

				if length < completed {
					length = completed
				}
			}

			if writeErr != nil {
				// nolint:wrapcheck
				return written, writeErr
			}

			if readBytes != wroteBytes {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			// nolint:wrapcheck
			return written, readErr
		}
	}

	return written, nil
}

// newDirEntryWithSummary makes DirEntry objects for directory Entries that need a DirectorySummary.
func newDirEntryWithSummary(d fs.Entry, oid object.ID, summ *fs.DirectorySummary) (*snapshot.DirEntry, error) {
	de, err := newDirEntry(d, oid)
	if err != nil {
		return nil, err
	}

	de.DirSummary = summ

	return de, nil
}

// newDirEntry makes DirEntry objects for any type of Entry.
func newDirEntry(md fs.Entry, oid object.ID) (*snapshot.DirEntry, error) {
	var entryType snapshot.EntryType

	switch md := md.(type) {
	case fs.Directory:
		entryType = snapshot.EntryTypeDirectory
	case fs.Symlink:
		entryType = snapshot.EntryTypeSymlink
	case fs.File, fs.StreamingFile:
		entryType = snapshot.EntryTypeFile
	default:
		return nil, errors.Errorf("invalid entry type %T", md)
	}

	return &snapshot.DirEntry{
		Name:        md.Name(),
		Type:        entryType,
		Permissions: snapshot.Permissions(md.Mode() & os.ModePerm),
		FileSize:    md.Size(),
		ModTime:     md.ModTime(),
		UserID:      md.Owner().UserID,
		GroupID:     md.Owner().GroupID,
		ObjectID:    oid,
	}, nil
}

// uploadFileWithCheckpointing uploads the specified File to the repository.
func (u *Uploader) uploadFileWithCheckpointing(ctx context.Context, relativePath string, file fs.File, pol *policy.Policy, sourceInfo snapshot.SourceInfo) (*snapshot.DirEntry, error) {
	par := u.effectiveParallelFileReads(pol)
	if par == 1 {
		par = 0
	}

	var cp checkpointRegistry

	cancelCheckpointer := u.periodicallyCheckpoint(ctx, &cp, &snapshot.Manifest{Source: sourceInfo})
	defer cancelCheckpointer()

	res, err := u.uploadFileInternal(ctx, &cp, relativePath, file, pol, par)
	if err != nil {
		return nil, err
	}

	return newDirEntryWithSummary(file, res.ObjectID, &fs.DirectorySummary{
		TotalFileCount: 1,
		TotalFileSize:  res.FileSize,
		MaxModTime:     res.ModTime,
	})
}

// checkpointRoot invokes checkpoints on the provided registry and if a checkpoint entry was generated,
// saves it in an incomplete snapshot manifest.
func (u *Uploader) checkpointRoot(ctx context.Context, cp *checkpointRegistry, prototypeManifest *snapshot.Manifest) error {
	var dmbCheckpoint dirManifestBuilder
	if err := cp.runCheckpoints(&dmbCheckpoint); err != nil {
		return errors.Wrap(err, "running checkpointers")
	}

	checkpointManifest := dmbCheckpoint.Build(u.repo.Time(), "dummy")
	if len(checkpointManifest.Entries) == 0 {
		// did not produce a checkpoint, that's ok
		return nil
	}

	if len(checkpointManifest.Entries) > 1 {
		return errors.Errorf("produced more than one checkpoint: %v", len(checkpointManifest.Entries))
	}

	rootEntry := checkpointManifest.Entries[0]

	uploadLog(ctx).Debugf("checkpointed root %v", rootEntry.ObjectID)

	man := *prototypeManifest
	man.RootEntry = rootEntry
	man.EndTime = u.repo.Time()
	man.StartTime = man.EndTime
	man.IncompleteReason = IncompleteReasonCheckpoint

	if _, err := snapshot.SaveSnapshot(ctx, u.repo, &man); err != nil {
		return errors.Wrap(err, "error saving checkpoint snapshot")
	}

	if _, err := policy.ApplyRetentionPolicy(ctx, u.repo, man.Source, true); err != nil {
		return errors.Wrap(err, "unable to apply retention policy")
	}

	if err := u.repo.Flush(ctx); err != nil {
		return errors.Wrap(err, "error flushing after checkpoint")
	}

	return nil
}

// periodicallyCheckpoint periodically (every CheckpointInterval) invokes checkpointRoot until the
// returned cancelation function has been called.
func (u *Uploader) periodicallyCheckpoint(ctx context.Context, cp *checkpointRegistry, prototypeManifest *snapshot.Manifest) (cancelFunc func()) {
	shutdown := make(chan struct{})
	ch := u.getTicker(u.CheckpointInterval)

	go func() {
		for {
			select {
			case <-shutdown:
				return

			case <-ch:
				if err := u.checkpointRoot(ctx, cp, prototypeManifest); err != nil {
					uploadLog(ctx).Errorf("error checkpointing: %v", err)
					u.Cancel()

					return
				}

				// test action
				if u.checkpointFinished != nil {
					u.checkpointFinished <- struct{}{}
				}
			}
		}
	}()

	return func() {
		close(shutdown)
	}
}

// uploadDirWithCheckpointing uploads the specified Directory to the repository.
func (u *Uploader) uploadDirWithCheckpointing(ctx context.Context, rootDir fs.Directory, policyTree *policy.Tree, previousDirs []fs.Directory, sourceInfo snapshot.SourceInfo) (*snapshot.DirEntry, error) {
	var (
		dmb dirManifestBuilder
		cp  checkpointRegistry
	)

	cancelCheckpointer := u.periodicallyCheckpoint(ctx, &cp, &snapshot.Manifest{Source: sourceInfo})
	defer cancelCheckpointer()

	var hc actionContext

	localDirPathOrEmpty := rootDir.LocalFilesystemPath()

	overrideDir, err := u.executeBeforeFolderAction(ctx, "before-snapshot-root", policyTree.EffectivePolicy().Actions.BeforeSnapshotRoot, localDirPathOrEmpty, &hc)
	if err != nil {
		return nil, dirReadError{errors.Wrap(err, "error executing before-snapshot-root action")}
	}

	if overrideDir != nil {
		rootDir = u.wrapIgnorefs(uploadLog(ctx), overrideDir, policyTree, true)
	}

	defer u.executeAfterFolderAction(ctx, "after-snapshot-root", policyTree.EffectivePolicy().Actions.AfterSnapshotRoot, localDirPathOrEmpty, &hc)

	return uploadDirInternal(ctx, u, rootDir, policyTree, previousDirs, localDirPathOrEmpty, ".", &dmb, &cp)
}

type uploadWorkItem struct {
	err error
}

func (u *Uploader) foreachEntryUnlessCanceled(ctx context.Context, wg *workshare.AsyncGroup, relativePath string, entries fs.Entries, cb func(ctx context.Context, entry fs.Entry, entryRelativePath string) error) error {
	for _, entry := range entries {
		entry := entry

		if u.IsCanceled() {
			return errCanceled
		}

		entryRelativePath := path.Join(relativePath, entry.Name())

		if wg.CanShareWork(u.workerPool) {
			wg.RunAsync(u.workerPool, func(c *workshare.Pool, input interface{}) {
				wi, _ := input.(*uploadWorkItem)
				wi.err = cb(ctx, entry, entryRelativePath)
			}, &uploadWorkItem{})
		} else {
			if err := cb(ctx, entry, entryRelativePath); err != nil {
				return err
			}
		}
	}

	return nil
}

func rootCauseError(err error) error {
	err = errors.Cause(err)

	var oserr *os.PathError
	if errors.As(err, &oserr) {
		err = oserr.Err
	}

	return err
}

type dirManifestBuilder struct {
	mu sync.Mutex

	// +checklocks:mu
	summary fs.DirectorySummary
	// +checklocks:mu
	entries []*snapshot.DirEntry
}

// Clone clones the current state of dirManifestBuilder.
func (b *dirManifestBuilder) Clone() *dirManifestBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	return &dirManifestBuilder{
		summary: b.summary.Clone(),
		entries: append([]*snapshot.DirEntry(nil), b.entries...),
	}
}

func (b *dirManifestBuilder) addEntry(de *snapshot.DirEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.entries = append(b.entries, de)

	if de.ModTime.After(b.summary.MaxModTime) {
		b.summary.MaxModTime = de.ModTime
	}

	// nolint:exhaustive
	switch de.Type {
	case snapshot.EntryTypeSymlink:
		b.summary.TotalSymlinkCount++

	case snapshot.EntryTypeFile:
		b.summary.TotalFileCount++
		b.summary.TotalFileSize += de.FileSize

	case snapshot.EntryTypeDirectory:
		if childSummary := de.DirSummary; childSummary != nil {
			b.summary.TotalFileCount += childSummary.TotalFileCount
			b.summary.TotalFileSize += childSummary.TotalFileSize
			b.summary.TotalDirCount += childSummary.TotalDirCount
			b.summary.FatalErrorCount += childSummary.FatalErrorCount
			b.summary.IgnoredErrorCount += childSummary.IgnoredErrorCount
			b.summary.FailedEntries = append(b.summary.FailedEntries, childSummary.FailedEntries...)

			if childSummary.MaxModTime.After(b.summary.MaxModTime) {
				b.summary.MaxModTime = childSummary.MaxModTime
			}
		}
	}
}

func (b *dirManifestBuilder) addFailedEntry(relPath string, isIgnoredError bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if isIgnoredError {
		b.summary.IgnoredErrorCount++
	} else {
		b.summary.FatalErrorCount++
	}

	b.summary.FailedEntries = append(b.summary.FailedEntries, &fs.EntryWithError{
		EntryPath: relPath,
		Error:     err.Error(),
	})
}

func (b *dirManifestBuilder) Build(dirModTime time.Time, incompleteReason string) *snapshot.DirManifest {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := b.summary
	s.TotalDirCount++

	entries := b.entries

	if len(entries) == 0 {
		s.MaxModTime = dirModTime
	}

	s.IncompleteReason = incompleteReason

	b.summary.FailedEntries = sortedTopFailures(b.summary.FailedEntries)

	// sort the result, directories first, then non-directories, ordered by name
	sort.Slice(b.entries, func(i, j int) bool {
		if leftDir, rightDir := isDir(entries[i]), isDir(entries[j]); leftDir != rightDir {
			// directories get sorted before non-directories
			return leftDir
		}

		return entries[i].Name < entries[j].Name
	})

	return &snapshot.DirManifest{
		StreamType: directoryStreamType,
		Summary:    &s,
		Entries:    entries,
	}
}

func sortedTopFailures(entries []*fs.EntryWithError) []*fs.EntryWithError {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].EntryPath < entries[j].EntryPath
	})

	if len(entries) > fs.MaxFailedEntriesPerDirectorySummary {
		entries = entries[0:fs.MaxFailedEntriesPerDirectorySummary]
	}

	return entries
}

func isDir(e *snapshot.DirEntry) bool {
	return e.Type == snapshot.EntryTypeDirectory
}

func (u *Uploader) processChildren(
	ctx context.Context,
	parentDirCheckpointRegistry *checkpointRegistry,
	parentDirBuilder *dirManifestBuilder,
	localDirPathOrEmpty, relativePath string,
	entries fs.Entries,
	policyTree *policy.Tree,
	previousEntries []fs.Entries,
) error {
	var wg workshare.AsyncGroup

	// ignore errCancel because a more serious error may be reported in wg.Wait()
	// we'll check for cancelation later.

	if err := u.processSubdirectories(ctx, parentDirCheckpointRegistry, parentDirBuilder, localDirPathOrEmpty, relativePath, entries, policyTree, previousEntries, &wg); err != nil && !errors.Is(err, errCanceled) {
		return errors.Wrap(err, "processing subdirectories")
	}

	if err := u.processNonDirectories(ctx, parentDirCheckpointRegistry, parentDirBuilder, relativePath, entries, policyTree, previousEntries, &wg); err != nil && !errors.Is(err, errCanceled) {
		return errors.Wrap(err, "processing non-directories")
	}

	for _, wi := range wg.Wait() {
		wi, ok := wi.(*uploadWorkItem)
		if !ok {
			return errors.Errorf("unexpected work item type %T", wi)
		}

		if wi.err != nil {
			return wi.err
		}
	}

	if u.IsCanceled() {
		return errCanceled
	}

	return nil
}

func (u *Uploader) processSubdirectories(
	ctx context.Context,
	parentDirCheckpointRegistry *checkpointRegistry,
	parentDirBuilder *dirManifestBuilder,
	localDirPathOrEmpty, relativePath string,
	entries fs.Entries,
	policyTree *policy.Tree,
	previousEntries []fs.Entries,
	wg *workshare.AsyncGroup,
) error {
	return u.foreachEntryUnlessCanceled(ctx, wg, relativePath, entries, func(ctx context.Context, entry fs.Entry, entryRelativePath string) error {
		dir, ok := entry.(fs.Directory)
		if !ok {
			// skip non-directories
			return nil
		}

		var previousDirs []fs.Directory
		for _, e := range previousEntries {
			if d, _ := e.FindByName(entry.Name()).(fs.Directory); d != nil {
				previousDirs = append(previousDirs, d)
			}
		}

		previousDirs = uniqueDirectories(previousDirs)

		childDirBuilder := &dirManifestBuilder{}

		childLocalDirPathOrEmpty := ""
		if localDirPathOrEmpty != "" {
			childLocalDirPathOrEmpty = filepath.Join(localDirPathOrEmpty, entry.Name())
		}

		childTree := policyTree.Child(entry.Name())

		de, err := uploadDirInternal(ctx, u, dir, childTree, previousDirs, childLocalDirPathOrEmpty, entryRelativePath, childDirBuilder, parentDirCheckpointRegistry)
		if errors.Is(err, errCanceled) {
			return err
		}

		if err != nil {
			// Note: This only catches errors in subdirectories of the snapshot root, not on the snapshot
			// root itself. The intention is to always fail if the top level directory can't be read,
			// otherwise a meaningless, empty snapshot is created that can't be restored.

			var dre dirReadError
			if errors.As(err, &dre) {
				u.reportErrorAndMaybeCancel(dre.error,
					childTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreDirectoryErrors.OrDefault(false),
					parentDirBuilder,
					entryRelativePath)
			} else {
				return errors.Wrapf(err, "unable to process directory %q", entry.Name())
			}
		} else {
			parentDirBuilder.addEntry(de)
		}

		return nil
	})
}

func metadataEquals(e1, e2 fs.Entry) bool {
	if l, r := e1.ModTime(), e2.ModTime(); !l.Equal(r) {
		return false
	}

	if l, r := e1.Mode(), e2.Mode(); l != r {
		return false
	}

	if l, r := e1.Size(), e2.Size(); l != r {
		return false
	}

	if l, r := e1.Owner(), e2.Owner(); l != r {
		return false
	}

	return true
}

func findCachedEntry(ctx context.Context, entryRelativePath string, entry fs.Entry, prevEntries []fs.Entries, pol *policy.Tree) fs.Entry {
	var missedEntry fs.Entry

	for _, e := range prevEntries {
		if ent := e.FindByName(entry.Name()); ent != nil {
			if metadataEquals(entry, ent) {
				return ent
			}

			missedEntry = ent
		}
	}

	if missedEntry != nil {
		if pol.EffectivePolicy().LoggingPolicy.Entries.CacheMiss.OrDefault(policy.LogDetailNone) >= policy.LogDetailNormal {
			uploadLog(ctx).Debugw(
				"cache miss",
				"path", entryRelativePath,
				"mode", missedEntry.Mode().String(),
				"size", missedEntry.Size(),
				"mtime", missedEntry.ModTime())
		}
	}

	return nil
}

func (u *Uploader) maybeIgnoreCachedEntry(ctx context.Context, ent fs.Entry) fs.Entry {
	if h, ok := ent.(object.HasObjectID); ok {
		if 100*rand.Float64() < u.ForceHashPercentage { // nolint:gosec
			uploadLog(ctx).Debugw("re-hashing cached object", "oid", h.ObjectID())
			return nil
		}

		return ent
	}

	return nil
}

func (u *Uploader) effectiveParallelFileReads(pol *policy.Policy) int {
	p := u.ParallelUploads
	max := pol.UploadPolicy.MaxParallelFileReads.OrDefault(runtime.NumCPU())

	if p < 1 || p > max {
		return max
	}

	return p
}

// nolint:funlen
func (u *Uploader) processNonDirectories(
	ctx context.Context,
	parentCheckpointRegistry *checkpointRegistry,
	parentDirBuilder *dirManifestBuilder,
	dirRelativePath string,
	entries fs.Entries,
	policyTree *policy.Tree,
	prevEntries []fs.Entries,
	wg *workshare.AsyncGroup,
) error {
	workerCount := u.effectiveParallelFileReads(policyTree.EffectivePolicy())

	var asyncWritesPerFile int

	if len(entries) < workerCount {
		if len(entries) > 0 {
			asyncWritesPerFile = workerCount / len(entries)
			if asyncWritesPerFile == 1 {
				asyncWritesPerFile = 0
			}
		}
	}

	return u.foreachEntryUnlessCanceled(ctx, wg, dirRelativePath, entries, func(ctx context.Context, entry fs.Entry, entryRelativePath string) error {
		// note this function runs in parallel and updates 'u.stats', which must be done using atomic operations.
		if _, ok := entry.(fs.Directory); ok {
			// skip directories
			return nil
		}

		t0 := timetrack.StartTimer()

		// See if we had this name during either of previous passes.
		if cachedEntry := u.maybeIgnoreCachedEntry(ctx, findCachedEntry(ctx, entryRelativePath, entry, prevEntries, policyTree)); cachedEntry != nil {
			atomic.AddInt32(&u.stats.CachedFiles, 1)
			atomic.AddInt64(&u.stats.TotalFileSize, entry.Size())
			u.Progress.CachedFile(filepath.Join(dirRelativePath, entry.Name()), entry.Size())

			// compute entryResult now, cachedEntry is short-lived
			cachedDirEntry, err := newDirEntry(entry, cachedEntry.(object.HasObjectID).ObjectID())
			if err != nil {
				return errors.Wrap(err, "unable to create dir entry")
			}

			maybeLogEntryProcessed(
				uploadLog(ctx),
				u.OverrideEntryLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Entries.CacheHit.OrDefault(policy.LogDetailNone)),
				"cached", entryRelativePath, cachedDirEntry, nil, t0)

			parentDirBuilder.addEntry(cachedDirEntry)

			return nil
		}

		switch entry := entry.(type) {
		case fs.Symlink:
			de, err := u.uploadSymlinkInternal(ctx, entryRelativePath, entry)
			if err != nil {
				isIgnoredError := policyTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreFileErrors.OrDefault(false)

				u.reportErrorAndMaybeCancel(err, isIgnoredError, parentDirBuilder, entryRelativePath)
			} else {
				parentDirBuilder.addEntry(de)
			}

			maybeLogEntryProcessed(
				uploadLog(ctx),
				u.OverrideEntryLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Entries.Snapshotted.OrDefault(policy.LogDetailNone)),
				"snapshotted symlink", entryRelativePath, de, err, t0)

			return nil

		case fs.File:
			atomic.AddInt32(&u.stats.NonCachedFiles, 1)

			de, err := u.uploadFileInternal(ctx, parentCheckpointRegistry, entryRelativePath, entry, policyTree.Child(entry.Name()).EffectivePolicy(), asyncWritesPerFile)
			if err != nil {
				isIgnoredError := policyTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreFileErrors.OrDefault(false)

				u.reportErrorAndMaybeCancel(err, isIgnoredError, parentDirBuilder, entryRelativePath)
			} else {
				parentDirBuilder.addEntry(de)
			}

			maybeLogEntryProcessed(
				uploadLog(ctx),
				u.OverrideEntryLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Entries.Snapshotted.OrDefault(policy.LogDetailNone)),
				"snapshotted file", entryRelativePath, de, nil, t0)

			return nil

		case fs.ErrorEntry:
			var (
				isIgnoredError bool
				prefix         string
			)

			if errors.Is(entry.ErrorInfo(), fs.ErrUnknown) {
				isIgnoredError = policyTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreUnknownTypes.OrDefault(true)
				prefix = "unknown entry"
			} else {
				isIgnoredError = policyTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreFileErrors.OrDefault(false)
				prefix = "error"
			}

			maybeLogEntryProcessed(
				uploadLog(ctx),
				u.OverrideEntryLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Entries.Snapshotted.OrDefault(policy.LogDetailNone)),
				prefix, entryRelativePath, nil, entry.ErrorInfo(), t0)

			u.reportErrorAndMaybeCancel(entry.ErrorInfo(), isIgnoredError, parentDirBuilder, entryRelativePath)

			return nil

		case fs.StreamingFile:
			atomic.AddInt32(&u.stats.NonCachedFiles, 1)

			de, err := u.uploadStreamingFileInternal(ctx, entryRelativePath, entry)
			if err != nil {
				isIgnoredError := policyTree.EffectivePolicy().ErrorHandlingPolicy.IgnoreFileErrors.OrDefault(false)

				u.reportErrorAndMaybeCancel(err, isIgnoredError, parentDirBuilder, entryRelativePath)
			} else {
				parentDirBuilder.addEntry(de)
			}

			maybeLogEntryProcessed(
				uploadLog(ctx), u.OverrideEntryLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Entries.Snapshotted.OrDefault(policy.LogDetailNone)),
				"snapshotted streaming file", entryRelativePath, de, nil, t0)

			return nil

		default:
			return errors.Errorf("unexpected entry type: %T %v", entry, entry.Mode())
		}
	})
}

func maybeLogEntryProcessed(logger logging.Logger, level policy.LogDetail, msg, relativePath string, de *snapshot.DirEntry, err error, timer timetrack.Timer) {
	if level <= policy.LogDetailNone && err == nil {
		return
	}

	var (
		bitsBuf       [10]interface{}
		keyValuePairs = append(bitsBuf[:0], "path", relativePath)
	)

	if err != nil {
		keyValuePairs = append(keyValuePairs, "error", err.Error())
	}

	if level >= minDetailLevelDuration {
		keyValuePairs = append(keyValuePairs, "dur", timer.Elapsed())
	}

	// nolint:nestif
	if de != nil {
		if level >= minDetailLevelSize {
			if ds := de.DirSummary; ds != nil {
				keyValuePairs = append(keyValuePairs, "size", ds.TotalFileSize)
			} else {
				keyValuePairs = append(keyValuePairs, "size", de.FileSize)
			}
		}

		if level >= minDetailLevelDirStats {
			if ds := de.DirSummary; ds != nil {
				keyValuePairs = append(keyValuePairs,
					"files", ds.TotalFileCount,
					"dirs", ds.TotalDirCount,
					"errors", ds.IgnoredErrorCount+ds.FatalErrorCount,
				)
			}
		}

		if level >= minDetailLevelModTime {
			if ds := de.DirSummary; ds != nil {
				keyValuePairs = append(keyValuePairs,
					"mtime", ds.MaxModTime.Format(time.RFC3339),
				)
			} else {
				keyValuePairs = append(keyValuePairs,
					"mtime", de.ModTime.Format(time.RFC3339),
				)
			}
		}

		if level >= minDetailLevelOID {
			keyValuePairs = append(keyValuePairs, "oid", de.ObjectID)
		}
	}

	logger.Debugw(msg, keyValuePairs...)
}

func maybeReadDirectoryEntries(ctx context.Context, dir fs.Directory) fs.Entries {
	if dir == nil {
		return nil
	}

	ent, err := dir.Readdir(ctx)
	if err != nil {
		uploadLog(ctx).Errorf("unable to read previous directory entries: %v", err)
		return nil
	}

	return ent
}

func uniqueDirectories(dirs []fs.Directory) []fs.Directory {
	if len(dirs) <= 1 {
		return dirs
	}

	unique := map[object.ID]fs.Directory{}

	for _, dir := range dirs {
		if hoid, ok := dir.(object.HasObjectID); ok {
			unique[hoid.ObjectID()] = dir
		}
	}

	if len(unique) == len(dirs) {
		return dirs
	}

	var result []fs.Directory
	for _, d := range unique {
		result = append(result, d)
	}

	return result
}

// dirReadError distinguishes an error thrown when attempting to read a directory.
type dirReadError struct {
	error
}

func uploadShallowDirInternal(ctx context.Context, directory fs.Directory, u *Uploader) (*snapshot.DirEntry, error) {
	if pf, ok := directory.(snapshot.HasDirEntryOrNil); ok {
		switch de, err := pf.DirEntryOrNil(ctx); {
		case err != nil:
			return nil, errors.Wrapf(err, "error reading placeholder for %q", directory.Name())
		case err == nil && de != nil:
			if _, err := u.repo.VerifyObject(ctx, de.ObjectID); err != nil {
				return nil, errors.Wrapf(err, "invalid placeholder for %q contains foreign object.ID", directory.Name())
			}

			return de, nil
		}
	}
	// No placeholder file exists, proceed as before.
	return nil, nil
}

func uploadDirInternal(
	ctx context.Context,
	u *Uploader,
	directory fs.Directory,
	policyTree *policy.Tree,
	previousDirs []fs.Directory,
	localDirPathOrEmpty, dirRelativePath string,
	thisDirBuilder *dirManifestBuilder,
	thisCheckpointRegistry *checkpointRegistry,
) (resultDE *snapshot.DirEntry, resultErr error) {
	atomic.AddInt32(&u.stats.TotalDirectoryCount, 1)

	t0 := timetrack.StartTimer()

	defer func() {
		maybeLogEntryProcessed(
			uploadLog(ctx),
			u.OverrideDirLogDetail.OrDefault(policyTree.EffectivePolicy().LoggingPolicy.Directories.Snapshotted.OrDefault(policy.LogDetailNone)),
			"snapshotted directory", dirRelativePath, resultDE, resultErr, t0)
	}()

	u.Progress.StartedDirectory(dirRelativePath)
	defer u.Progress.FinishedDirectory(dirRelativePath)

	var definedActions policy.ActionsPolicy

	if p := policyTree.DefinedPolicy(); p != nil {
		definedActions = p.Actions
	}

	var hc actionContext
	defer cleanupActionContext(ctx, &hc)

	overrideDir, herr := u.executeBeforeFolderAction(ctx, "before-folder", definedActions.BeforeFolder, localDirPathOrEmpty, &hc)
	if herr != nil {
		return nil, dirReadError{errors.Wrap(herr, "error executing before-folder action")}
	}

	defer u.executeAfterFolderAction(ctx, "after-folder", definedActions.AfterFolder, localDirPathOrEmpty, &hc)

	if overrideDir != nil {
		directory = u.wrapIgnorefs(uploadLog(ctx), overrideDir, policyTree, true)
	}

	if de, err := uploadShallowDirInternal(ctx, directory, u); de != nil || err != nil {
		return de, err
	}

	entries, direrr := directory.Readdir(ctx)

	if direrr != nil {
		return nil, dirReadError{direrr}
	}

	var prevEntries []fs.Entries

	for _, d := range uniqueDirectories(previousDirs) {
		if ent := maybeReadDirectoryEntries(ctx, d); ent != nil {
			prevEntries = append(prevEntries, ent)
		}
	}

	childCheckpointRegistry := &checkpointRegistry{}

	thisCheckpointRegistry.addCheckpointCallback(directory, func() (*snapshot.DirEntry, error) {
		// when snapshotting the parent, snapshot all our children and tell them to populate
		// childCheckpointBuilder
		thisCheckpointBuilder := thisDirBuilder.Clone()

		// invoke all child checkpoints which will populate thisCheckpointBuilder.
		if err := childCheckpointRegistry.runCheckpoints(thisCheckpointBuilder); err != nil {
			return nil, errors.Wrapf(err, "error checkpointing children")
		}

		checkpointManifest := thisCheckpointBuilder.Build(directory.ModTime(), IncompleteReasonCheckpoint)
		oid, err := u.writeDirManifest(ctx, dirRelativePath, checkpointManifest)
		if err != nil {
			return nil, errors.Wrap(err, "error writing dir manifest")
		}

		return newDirEntryWithSummary(directory, oid, checkpointManifest.Summary)
	})
	defer thisCheckpointRegistry.removeCheckpointCallback(directory)

	if err := u.processChildren(ctx, childCheckpointRegistry, thisDirBuilder, localDirPathOrEmpty, dirRelativePath, entries, policyTree, prevEntries); err != nil && !errors.Is(err, errCanceled) {
		return nil, err
	}

	dirManifest := thisDirBuilder.Build(directory.ModTime(), u.incompleteReason())

	oid, err := u.writeDirManifest(ctx, dirRelativePath, dirManifest)
	if err != nil {
		return nil, errors.Wrapf(err, "error writing dir manifest: %v", directory.Name())
	}

	return newDirEntryWithSummary(directory, oid, dirManifest.Summary)
}

func (u *Uploader) writeDirManifest(ctx context.Context, dirRelativePath string, dirManifest *snapshot.DirManifest) (object.ID, error) {
	writer := u.repo.NewObjectWriter(ctx, object.WriterOptions{
		Description: "DIR:" + dirRelativePath,
		Prefix:      objectIDPrefixDirectory,
	})

	defer writer.Close() //nolint:errcheck

	if err := json.NewEncoder(writer).Encode(dirManifest); err != nil {
		return "", errors.Wrap(err, "unable to encode directory JSON")
	}

	oid, err := writer.Result()
	if err != nil {
		return "", errors.Wrap(err, "unable to write directory")
	}

	return oid, nil
}

func (u *Uploader) reportErrorAndMaybeCancel(err error, isIgnored bool, dmb *dirManifestBuilder, entryRelativePath string) {
	if u.IsCanceled() && errors.Is(err, errCanceled) {
		// alrady canceled, do not report another.
		return
	}

	if isIgnored {
		atomic.AddInt32(&u.stats.IgnoredErrorCount, 1)
	} else {
		atomic.AddInt32(&u.stats.ErrorCount, 1)
	}

	rc := rootCauseError(err)
	u.Progress.Error(entryRelativePath, rc, isIgnored)
	dmb.addFailedEntry(entryRelativePath, isIgnored, rc)

	if u.FailFast && !isIgnored {
		u.Cancel()
	}
}

// NewUploader creates new Uploader object for a given repository.
func NewUploader(r repo.RepositoryWriter) *Uploader {
	return &Uploader{
		repo:               r,
		Progress:           &NullUploadProgress{},
		EnableActions:      r.ClientOptions().EnableActions,
		CheckpointInterval: DefaultCheckpointInterval,
		getTicker:          time.Tick,
	}
}

// Cancel requests cancellation of an upload that's in progress. Will typically result in an incomplete snapshot.
func (u *Uploader) Cancel() {
	atomic.StoreInt32(&u.canceled, 1)
}

func (u *Uploader) maybeOpenDirectoryFromManifest(ctx context.Context, man *snapshot.Manifest) fs.Directory {
	if man == nil {
		return nil
	}

	ent := EntryFromDirEntry(u.repo, man.RootEntry)

	dir, ok := ent.(fs.Directory)
	if !ok {
		uploadLog(ctx).Debugf("previous manifest root is not a directory (was %T %+v)", ent, man.RootEntry)
		return nil
	}

	return dir
}

// Upload uploads contents of the specified filesystem entry (file or directory) to the repository and returns snapshot.Manifest with statistics.
// Old snapshot manifest, when provided can be used to speed up uploads by utilizing hash cache.
func (u *Uploader) Upload(
	ctx context.Context,
	source fs.Entry,
	policyTree *policy.Tree,
	sourceInfo snapshot.SourceInfo,
	previousManifests ...*snapshot.Manifest,
) (*snapshot.Manifest, error) {
	u.Progress.UploadStarted()
	defer u.Progress.UploadFinished()

	parallel := u.effectiveParallelFileReads(policyTree.EffectivePolicy())

	uploadLog(ctx).Debugf("Uploading %v with parallelism %v", sourceInfo, parallel)

	s := &snapshot.Manifest{
		Source: sourceInfo,
	}

	u.workerPool = workshare.NewPool(parallel - 1)
	defer u.workerPool.Close()

	u.stats = &snapshot.Stats{}
	atomic.StoreInt64(&u.totalWrittenBytes, 0)

	var err error

	s.StartTime = u.repo.Time()

	var scanWG sync.WaitGroup

	scanctx, cancelScan := context.WithCancel(ctx)

	defer cancelScan()

	switch entry := source.(type) {
	case fs.Directory:
		var previousDirs []fs.Directory

		for _, m := range previousManifests {
			if d := u.maybeOpenDirectoryFromManifest(ctx, m); d != nil {
				previousDirs = append(previousDirs, d)
			}
		}

		scanWG.Add(1)

		go func() {
			defer scanWG.Done()

			wrapped := u.wrapIgnorefs(estimateLog(ctx), entry, policyTree, false /* reportIgnoreStats */)

			ds, _ := u.scanDirectory(scanctx, wrapped, policyTree)

			u.Progress.EstimatedDataSize(ds.numFiles, ds.totalFileSize)
		}()

		wrapped := u.wrapIgnorefs(uploadLog(ctx), entry, policyTree, true /* reportIgnoreStats */)

		s.RootEntry, err = u.uploadDirWithCheckpointing(ctx, wrapped, policyTree, previousDirs, sourceInfo)

	case fs.File:
		u.Progress.EstimatedDataSize(1, entry.Size())
		s.RootEntry, err = u.uploadFileWithCheckpointing(ctx, entry.Name(), entry, policyTree.EffectivePolicy(), sourceInfo)

	default:
		return nil, errors.Errorf("unsupported source: %v", s.Source)
	}

	if err != nil {
		return nil, err
	}

	cancelScan()
	scanWG.Wait()

	s.IncompleteReason = u.incompleteReason()
	s.EndTime = u.repo.Time()
	s.Stats = *u.stats

	return s, nil
}

func (u *Uploader) wrapIgnorefs(logger logging.Logger, entry fs.Directory, policyTree *policy.Tree, reportIgnoreStats bool) fs.Directory {
	if u.DisableIgnoreRules {
		return entry
	}

	return ignorefs.New(entry, policyTree, ignorefs.ReportIgnoredFiles(func(ctx context.Context, fname string, md fs.Entry, policyTree *policy.Tree) {
		if md.IsDir() {
			maybeLogEntryProcessed(
				logger,
				policyTree.EffectivePolicy().LoggingPolicy.Directories.Ignored.OrDefault(policy.LogDetailNone),
				"ignored directory", fname, nil, nil, timetrack.StartTimer())

			if reportIgnoreStats {
				u.Progress.ExcludedDir(fname)
			}
		} else {
			maybeLogEntryProcessed(
				logger,
				policyTree.EffectivePolicy().LoggingPolicy.Entries.Ignored.OrDefault(policy.LogDetailNone),
				"ignored", fname, nil, nil, timetrack.StartTimer())

			if reportIgnoreStats {
				u.Progress.ExcludedFile(fname, md.Size())
			}
		}

		u.stats.AddExcluded(md)
	}))
}
