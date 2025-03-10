package index

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/compression"
)

const (
	invalidBlobID              = "---invalid---"
	invalidFormatVersion       = 0xFF
	invalidCompressionHeaderID = 0xFFFF
	invalidEncryptionKeyID     = 0xFF
)

const (
	// Version2 identifies version 2 of the index, supporting content-level compression.
	Version2 = 2

	v2IndexHeaderSize       = 17 // size of fixed header at the beginning of index
	v2PackInfoSize          = 5  // size of each pack information blob
	v2MaxFormatCount        = invalidFormatVersion
	v2MaxUniquePackIDCount  = 1 << 24 // max number of packs that can be stored
	v2MaxShortPackIDCount   = 1 << 16 // max number that can be represented using 2 bytes
	v2MaxContentLength      = 1 << 28 // max supported content length (representible using 3.5 bytes)
	v2MaxShortContentLength = 1 << 24 // max content length representible using 3 bytes
	v2MaxPackOffset         = 1 << 30 // max pack offset 1GiB to leave 2 bits for flags
	v2DeletedMarker         = 0x80000000
	v2MaxEntrySize          = 256 // maximum length of content ID + per-entry data combined
)

// layout of v2 index entry:
//    0-3: timestamp bits 0..31 (relative to base time)
//    4-7: pack offset bits 0..29
//         flags:
//            isDeleted                    (1 bit)
//   8-10: original length bits 0..23
//  11-13: packed length bits 0..23
//  14-15: pack ID (lower 16 bits)- index into Packs[]
//
// optional bytes:
//     16: format ID - index into Formats[] - 0 - present if not all formats are identical

//     17: pack ID - bits 16..23 - present if more than 2^16 packs are in a single index

//     18: high-order bits - present if any content length is greater than 2^24 == 16MiB
//            original length bits 24..27  (4 hi bits)
//            packed length bits 24..27    (4 lo bits)
const (
	v2EntryOffsetTimestampSeconds      = 0
	v2EntryOffsetPackOffsetAndFlags    = 4
	v2EntryOffsetOriginalLength        = 8
	v2EntryOffsetPackedLength          = 11
	v2EntryOffsetPackBlobID            = 14
	v2EntryMinLength                   = v2EntryOffsetFormatID
	v2EntryOffsetFormatID              = 16 // optional, assumed zero if missing
	v2EntryOffsetFormatIDEnd           = v2EntryOffsetExtendedPackBlobID
	v2EntryOffsetExtendedPackBlobID    = 17 // optional
	v2EntryOffsetExtendedPackBlobIDEnd = v2EntryOffsetHighLengthBits
	v2EntryOffsetHighLengthBits        = 18 // optional
	v2EntryOffsetHighLengthBitsEnd     = v2EntryMaxLength
	v2EntryMaxLength                   = 19

	// flags (at offset v2EntryOffsetPackOffsetAndFlags).
	v2EntryDeletedFlag    = 0x80
	v2EntryPackOffsetMask = 1<<31 - 1

	// high bits (at offset v2EntryOffsetHighLengthBits).
	v2EntryHighLengthShift                   = 24
	v2EntryHighLengthBitsOriginalLengthShift = 4
	v2EntryHghLengthBitsPackedLengthMask     = 0xF

	// PackBlobID bits above 16 are stored in separate byte.
	v2EntryExtendedPackBlobIDShift = 16
)

// layout of v2 format entry
//    0-3: compressionID - 32 bit (corresponding to compression.HeaderID)
//
const (
	v2FormatInfoSize = 6

	v2FormatOffsetCompressionID   = 0
	v2FormatOffsetFormatVersion   = 4
	v2FormatOffsetEncryptionKeyID = 5
)

// FormatV2 describes a format of a single pack index. The actual structure is not used,
// it's purely for documentation purposes.
// The struct is byte-aligned.
type FormatV2 struct {
	Header struct {
		Version           byte   // format version number must be 0x02
		KeySize           byte   // size of each key in bytes
		EntrySize         uint16 // size of each entry in bytes, big-endian
		EntryCount        uint32 // number of sorted (key,value) entries that follow
		EntriesOffset     uint32 // offset where `Entries` begins
		FormatInfosOffset uint32 // offset where `Formats` begins
		NumFormatInfos    uint32
		PacksOffset       uint32 // offset where `Packs` begins
		NumPacks          uint32
		BaseTimestamp     uint32 // base timestamp in unix seconds
	}

	Entries []struct {
		Key   []byte // key bytes (KeySize)
		Entry indexV2EntryInfo
	}

	// each entry contains offset+length of the name of the pack blob, so that each entry can refer to the index
	// and it resolves to a name.
	Packs []struct {
		PackNameLength byte   // length of the filename
		PackNameOffset uint32 // offset to data (within extra data)
	}

	// each entry represents unique content format.
	Formats []indexV2FormatInfo

	ExtraData []byte // extra data
}

type indexV2FormatInfo struct {
	compressionHeaderID compression.HeaderID
	formatVersion       byte
	encryptionKeyID     byte
}

type indexV2EntryInfo struct {
	data      string // basically a byte array, but immutable
	contentID ID
	b         *indexV2
}

func (e indexV2EntryInfo) GetContentID() ID {
	return e.contentID
}

func (e indexV2EntryInfo) GetTimestampSeconds() int64 {
	return int64(decodeBigEndianUint32(e.data[v2EntryOffsetTimestampSeconds:])) + int64(e.b.hdr.baseTimestamp)
}

func (e indexV2EntryInfo) GetDeleted() bool {
	return e.data[v2EntryOffsetPackOffsetAndFlags]&v2EntryDeletedFlag != 0
}

func (e indexV2EntryInfo) GetPackOffset() uint32 {
	return decodeBigEndianUint32(e.data[v2EntryOffsetPackOffsetAndFlags:]) & v2EntryPackOffsetMask
}

func (e indexV2EntryInfo) GetOriginalLength() uint32 {
	v := decodeBigEndianUint24(e.data[v2EntryOffsetOriginalLength:])
	if len(e.data) > v2EntryOffsetHighLengthBits {
		v |= uint32(e.data[v2EntryOffsetHighLengthBits]>>v2EntryHighLengthBitsOriginalLengthShift) << v2EntryHighLengthShift
	}

	return v
}

func (e indexV2EntryInfo) GetPackedLength() uint32 {
	v := decodeBigEndianUint24(e.data[v2EntryOffsetPackedLength:])
	if len(e.data) > v2EntryOffsetHighLengthBits {
		v |= uint32(e.data[v2EntryOffsetHighLengthBits]&v2EntryHghLengthBitsPackedLengthMask) << v2EntryHighLengthShift
	}

	return v
}

func (e indexV2EntryInfo) formatIDIndex() int {
	if len(e.data) > v2EntryOffsetFormatID {
		return int(e.data[v2EntryOffsetFormatID])
	}

	return 0
}

func (e indexV2EntryInfo) GetFormatVersion() byte {
	fid := e.formatIDIndex()
	if fid > len(e.b.formats) {
		return invalidFormatVersion
	}

	return e.b.formats[fid].formatVersion
}

func (e indexV2EntryInfo) GetCompressionHeaderID() compression.HeaderID {
	fid := e.formatIDIndex()
	if fid > len(e.b.formats) {
		return invalidCompressionHeaderID
	}

	return e.b.formats[fid].compressionHeaderID
}

func (e indexV2EntryInfo) GetEncryptionKeyID() byte {
	fid := e.formatIDIndex()
	if fid > len(e.b.formats) {
		return invalidEncryptionKeyID
	}

	return e.b.formats[fid].encryptionKeyID
}

func (e indexV2EntryInfo) GetPackBlobID() blob.ID {
	packIDIndex := uint32(decodeBigEndianUint16(e.data[v2EntryOffsetPackBlobID:]))
	if len(e.data) > v2EntryOffsetExtendedPackBlobID {
		packIDIndex |= uint32(e.data[v2EntryOffsetExtendedPackBlobID]) << v2EntryExtendedPackBlobIDShift
	}

	return e.b.getPackBlobIDByIndex(packIDIndex)
}

func (e indexV2EntryInfo) Timestamp() time.Time {
	return time.Unix(e.GetTimestampSeconds(), 0)
}

var _ Info = indexV2EntryInfo{}

type v2HeaderInfo struct {
	version       int
	keySize       int
	entrySize     int
	entryCount    int
	packCount     uint
	formatCount   byte
	baseTimestamp uint32 // base timestamp in unix seconds

	// calculated
	entriesOffset int64
	formatsOffset int64
	packsOffset   int64
	entryStride   int64 // guaranteed to be < v2MaxEntrySize
}

type indexV2 struct {
	hdr      v2HeaderInfo
	readerAt io.ReaderAt
	formats  []indexV2FormatInfo
}

func (b *indexV2) getPackBlobIDByIndex(ndx uint32) blob.ID {
	if ndx >= uint32(b.hdr.packCount) {
		return invalidBlobID
	}

	var buf [v2PackInfoSize]byte

	if err := readAtAll(b.readerAt, buf[:], b.hdr.packsOffset+int64(v2PackInfoSize*ndx)); err != nil {
		return invalidBlobID
	}

	nameLength := int(buf[0])
	nameOffset := binary.BigEndian.Uint32(buf[1:])

	var nameBuf [256]byte

	if err := readAtAll(b.readerAt, nameBuf[0:nameLength], int64(nameOffset)); err != nil {
		return invalidBlobID
	}

	return blob.ID(nameBuf[0:nameLength])
}

func (b *indexV2) ApproximateCount() int {
	return b.hdr.entryCount
}

// Iterate invokes the provided callback function for a range of contents in the index, sorted alphabetically.
// The iteration ends when the callback returns an error, which is propagated to the caller or when
// all contents have been visited.
func (b *indexV2) Iterate(r IDRange, cb func(Info) error) error {
	startPos, err := b.findEntryPosition(r.StartID)
	if err != nil {
		return errors.Wrap(err, "could not find starting position")
	}

	var entryBuf [v2MaxEntrySize]byte
	entry := entryBuf[0:b.hdr.entryStride]

	for i := startPos; i < b.hdr.entryCount; i++ {
		if err := readAtAll(b.readerAt, entry, b.entryOffset(i)); err != nil {
			return errors.Wrap(err, "unable to read from index")
		}

		key := entry[0:b.hdr.keySize]

		contentID := bytesToContentID(key)
		if contentID >= r.EndID {
			break
		}

		i, err := b.entryToInfo(contentID, entry[b.hdr.keySize:])
		if err != nil {
			return errors.Wrap(err, "invalid index data")
		}

		if err := cb(i); err != nil {
			return err
		}
	}

	return nil
}

func (b *indexV2) entryOffset(p int) int64 {
	return b.hdr.entriesOffset + b.hdr.entryStride*int64(p)
}

func (b *indexV2) findEntryPosition(contentID ID) (int, error) {
	var entryArr [v2MaxEntrySize]byte
	entryBuf := entryArr[0:b.hdr.entryStride]

	var readErr error

	pos := sort.Search(b.hdr.entryCount, func(p int) bool {
		if readErr != nil {
			return false
		}

		if err := readAtAll(b.readerAt, entryBuf, b.entryOffset(p)); err != nil {
			readErr = err
			return false
		}

		return bytesToContentID(entryBuf[0:b.hdr.keySize]) >= contentID
	})

	return pos, readErr
}

func (b *indexV2) findEntryPositionExact(idBytes, entryBuf []byte) (int, error) {
	var readErr error

	pos := sort.Search(b.hdr.entryCount, func(p int) bool {
		if readErr != nil {
			return false
		}

		if err := readAtAll(b.readerAt, entryBuf, b.entryOffset(p)); err != nil {
			readErr = err
			return false
		}

		return contentIDBytesGreaterOrEqual(entryBuf[0:b.hdr.keySize], idBytes)
	})

	return pos, readErr
}

func (b *indexV2) findEntry(output []byte, contentID ID) ([]byte, error) {
	var hashBuf [maxContentIDSize]byte

	key := contentIDToBytes(hashBuf[:0], contentID)

	// empty index blob, this is possible when compaction removes exactly everything
	if b.hdr.keySize == unknownKeySize {
		return nil, nil
	}

	if len(key) != b.hdr.keySize {
		return nil, errors.Errorf("invalid content ID: %q (%v vs %v)", contentID, len(key), b.hdr.keySize)
	}

	var entryArr [v2MaxEntrySize]byte
	entryBuf := entryArr[0:b.hdr.entryStride]

	position, err := b.findEntryPositionExact(key, entryBuf)
	if err != nil {
		return nil, err
	}

	if position >= b.hdr.entryCount {
		return nil, nil
	}

	if err := readAtAll(b.readerAt, entryBuf, b.entryOffset(position)); err != nil {
		return nil, errors.Wrap(err, "error reading header")
	}

	if bytes.Equal(entryBuf[0:len(key)], key) {
		return append(output, entryBuf[len(key):]...), nil
	}

	return nil, nil
}

// GetInfo returns information about a given content. If a content is not found, nil is returned.
func (b *indexV2) GetInfo(contentID ID) (Info, error) {
	var entryBuf [v2MaxEntrySize]byte

	e, err := b.findEntry(entryBuf[:0], contentID)
	if err != nil {
		return nil, err
	}

	if e == nil {
		return nil, nil
	}

	return b.entryToInfo(contentID, e)
}

func (b *indexV2) entryToInfo(contentID ID, entryData []byte) (Info, error) {
	if len(entryData) < v2EntryMinLength {
		return nil, errors.Errorf("invalid entry length: %v", len(entryData))
	}

	// convert to 'entryData' string to make it read-only
	return indexV2EntryInfo{string(entryData), contentID, b}, nil
}

// Close closes the index and the underlying reader.
func (b *indexV2) Close() error {
	if closer, ok := b.readerAt.(io.Closer); ok {
		return errors.Wrap(closer.Close(), "error closing index file")
	}

	return nil
}

type indexBuilderV2 struct {
	packBlobIDOffsets      map[blob.ID]uint32
	entryCount             int
	keyLength              int
	entrySize              int
	extraDataOffset        uint32
	uniqueFormatInfo2Index map[indexV2FormatInfo]byte
	packID2Index           map[blob.ID]int
	baseTimestamp          int64
}

func indexV2FormatInfoFromInfo(v Info) indexV2FormatInfo {
	return indexV2FormatInfo{
		formatVersion:       v.GetFormatVersion(),
		compressionHeaderID: v.GetCompressionHeaderID(),
		encryptionKeyID:     v.GetEncryptionKeyID(),
	}
}

// buildUniqueFormatToIndexMap builds a map of unique indexV2FormatInfo to their numeric identifiers.
func buildUniqueFormatToIndexMap(sortedInfos []Info) map[indexV2FormatInfo]byte {
	result := map[indexV2FormatInfo]byte{}

	for _, v := range sortedInfos {
		key := indexV2FormatInfoFromInfo(v)
		if _, ok := result[key]; !ok {
			result[key] = byte(len(result))
		}
	}

	return result
}

// buildPackIDToIndexMap builds a map of unqiue blob IDs to their numeric identifiers.
func buildPackIDToIndexMap(sortedInfos []Info) map[blob.ID]int {
	result := map[blob.ID]int{}

	for _, v := range sortedInfos {
		blobID := v.GetPackBlobID()
		if _, ok := result[blobID]; !ok {
			result[blobID] = len(result)
		}
	}

	return result
}

// maxContentLengths computes max content lengths in the builder.
func maxContentLengths(sortedInfos []Info) (maxPackedLength, maxOriginalLength, maxPackOffset uint32) {
	for _, v := range sortedInfos {
		if l := v.GetPackedLength(); l > maxPackedLength {
			maxPackedLength = l
		}

		if l := v.GetOriginalLength(); l > maxOriginalLength {
			maxOriginalLength = l
		}

		if l := v.GetPackOffset(); l > maxPackOffset {
			maxPackOffset = l
		}
	}

	return
}

func max(a, b int) int {
	if a > b {
		return a
	}

	return b
}

func newIndexBuilderV2(sortedInfos []Info) (*indexBuilderV2, error) {
	entrySize := v2EntryOffsetFormatID

	// compute a map of unique formats to their indexes.
	uniqueFormat2Index := buildUniqueFormatToIndexMap(sortedInfos)
	if len(uniqueFormat2Index) > v2MaxFormatCount {
		return nil, errors.Errorf("unsupported - too many unique formats %v (max %v)", len(uniqueFormat2Index), v2MaxFormatCount)
	}

	// if have more than one format present, we need to store per-entry format identifier, otherwise assume 0.
	if len(uniqueFormat2Index) > 1 {
		entrySize = max(entrySize, v2EntryOffsetFormatIDEnd)
	}

	packID2Index := buildPackIDToIndexMap(sortedInfos)
	if len(packID2Index) > v2MaxUniquePackIDCount {
		return nil, errors.Errorf("unsupported - too many unique pack IDs %v (max %v)", len(packID2Index), v2MaxUniquePackIDCount)
	}

	if len(packID2Index) > v2MaxShortPackIDCount {
		entrySize = max(entrySize, v2EntryOffsetExtendedPackBlobIDEnd)
	}

	// compute maximum content length to determine how many bits we need to use to store it.
	maxPackedLen, maxOriginalLength, maxPackOffset := maxContentLengths(sortedInfos)

	// contents >= 28 bits (256 MiB) can't be stored at all.
	if maxPackedLen >= v2MaxContentLength || maxOriginalLength >= v2MaxContentLength {
		return nil, errors.Errorf("maximum content length is too high: (packed %v, original %v, max %v)", maxPackedLen, maxOriginalLength, v2MaxContentLength)
	}

	// contents >= 24 bits (16 MiB) requires extra 0.5 byte per length.
	if maxPackedLen >= v2MaxShortContentLength || maxOriginalLength >= v2MaxShortContentLength {
		entrySize = max(entrySize, v2EntryOffsetHighLengthBitsEnd)
	}

	if maxPackOffset >= v2MaxPackOffset {
		return nil, errors.Errorf("pack offset %v is too high", maxPackOffset)
	}

	keyLength := -1

	if len(sortedInfos) > 0 {
		var hashBuf [maxContentIDSize]byte

		keyLength = len(contentIDToBytes(hashBuf[:0], sortedInfos[0].GetContentID()))
	}

	return &indexBuilderV2{
		packBlobIDOffsets:      map[blob.ID]uint32{},
		keyLength:              keyLength,
		entrySize:              entrySize,
		entryCount:             len(sortedInfos),
		uniqueFormatInfo2Index: uniqueFormat2Index,
		packID2Index:           packID2Index,
	}, nil
}

// buildV2 writes the pack index to the provided output.
func (b Builder) buildV2(output io.Writer) error {
	sortedInfos := b.sortedContents()

	b2, err := newIndexBuilderV2(sortedInfos)
	if err != nil {
		return err
	}

	w := bufio.NewWriter(output)

	// prepare extra data to be appended at the end of an index.
	extraData := b2.prepareExtraData(sortedInfos)

	if b2.keyLength <= 1 {
		return errors.Errorf("invalid key length: %v for %v", b2.keyLength, len(b))
	}

	// write header
	header := make([]byte, v2IndexHeaderSize)
	header[0] = Version2 // version
	header[1] = byte(b2.keyLength)
	binary.BigEndian.PutUint16(header[2:4], uint16(b2.entrySize))
	binary.BigEndian.PutUint32(header[4:8], uint32(b2.entryCount))
	binary.BigEndian.PutUint32(header[8:12], uint32(len(b2.packID2Index)))
	header[12] = byte(len(b2.uniqueFormatInfo2Index))
	binary.BigEndian.PutUint32(header[13:17], uint32(b2.baseTimestamp))

	if _, err := w.Write(header); err != nil {
		return errors.Wrap(err, "unable to write header")
	}

	// write sorted index entries
	for _, it := range sortedInfos {
		if err := b2.writeIndexEntry(w, it); err != nil {
			return errors.Wrap(err, "unable to write entry")
		}
	}

	// write pack ID entries in the index order of values from packID2Index (0, 1, 2, ...).
	reversePackIDIndex := make([]blob.ID, len(b2.packID2Index))
	for k, v := range b2.packID2Index {
		reversePackIDIndex[v] = k
	}

	// emit pack ID information in this order.
	for _, e := range reversePackIDIndex {
		if err := b2.writePackIDEntry(w, e); err != nil {
			return errors.Wrap(err, "error writing format info entry")
		}
	}

	// build a list of indexV2FormatInfo using the order of indexes from uniqueFormatInfo2Index.
	reverseFormatInfoIndex := make([]indexV2FormatInfo, len(b2.uniqueFormatInfo2Index))
	for k, v := range b2.uniqueFormatInfo2Index {
		reverseFormatInfoIndex[v] = k
	}

	// emit format information in this order.
	for _, f := range reverseFormatInfoIndex {
		if err := b2.writeFormatInfoEntry(w, f); err != nil {
			return errors.Wrap(err, "error writing format info entry")
		}
	}

	if _, err := w.Write(extraData); err != nil {
		return errors.Wrap(err, "error writing extra data")
	}

	return errors.Wrap(w.Flush(), "error flushing index")
}

func (b *indexBuilderV2) prepareExtraData(sortedInfos []Info) []byte {
	var extraData []byte

	for _, it := range sortedInfos {
		if it.GetPackBlobID() != "" {
			if _, ok := b.packBlobIDOffsets[it.GetPackBlobID()]; !ok {
				b.packBlobIDOffsets[it.GetPackBlobID()] = uint32(len(extraData))
				extraData = append(extraData, []byte(it.GetPackBlobID())...)
			}
		}
	}

	b.extraDataOffset = v2IndexHeaderSize                                         // fixed header
	b.extraDataOffset += uint32(b.entryCount * (b.keyLength + b.entrySize))       // entries index
	b.extraDataOffset += uint32(len(b.packID2Index) * v2PackInfoSize)             // pack information
	b.extraDataOffset += uint32(len(b.uniqueFormatInfo2Index) * v2FormatInfoSize) // formats

	return extraData
}

func (b *indexBuilderV2) writeIndexEntry(w io.Writer, it Info) error {
	var hashBuf [maxContentIDSize]byte

	k := contentIDToBytes(hashBuf[:0], it.GetContentID())

	if len(k) != b.keyLength {
		return errors.Errorf("inconsistent key length: %v vs %v", len(k), b.keyLength)
	}

	if _, err := w.Write(k); err != nil {
		return errors.Wrap(err, "error writing entry key")
	}

	if err := b.writeIndexValueEntry(w, it); err != nil {
		return errors.Wrap(err, "error writing entry")
	}

	return nil
}

func (b *indexBuilderV2) writePackIDEntry(w io.Writer, packID blob.ID) error {
	var buf [v2PackInfoSize]byte

	buf[0] = byte(len(packID))
	binary.BigEndian.PutUint32(buf[1:], b.packBlobIDOffsets[packID]+b.extraDataOffset)

	_, err := w.Write(buf[:])

	return errors.Wrap(err, "error writing pack ID entry")
}

func (b *indexBuilderV2) writeFormatInfoEntry(w io.Writer, f indexV2FormatInfo) error {
	var buf [v2FormatInfoSize]byte

	binary.BigEndian.PutUint32(buf[v2FormatOffsetCompressionID:], uint32(f.compressionHeaderID))
	buf[v2FormatOffsetFormatVersion] = f.formatVersion
	buf[v2FormatOffsetEncryptionKeyID] = f.encryptionKeyID

	_, err := w.Write(buf[:])

	return errors.Wrap(err, "error writing format info entry")
}

func (b *indexBuilderV2) writeIndexValueEntry(w io.Writer, it Info) error {
	var buf [v2EntryMaxLength]byte

	//    0-3: timestamp bits 0..31 (relative to base time)

	binary.BigEndian.PutUint32(
		buf[v2EntryOffsetTimestampSeconds:],
		uint32(it.GetTimestampSeconds()-b.baseTimestamp))

	//    4-7: pack offset bits 0..29
	//         flags:
	//            isDeleted                    (1 bit)

	packOffsetAndFlags := it.GetPackOffset()
	if it.GetDeleted() {
		packOffsetAndFlags |= v2DeletedMarker
	}

	binary.BigEndian.PutUint32(buf[v2EntryOffsetPackOffsetAndFlags:], packOffsetAndFlags)

	//   8-10: original length bits 0..23

	encodeBigEndianUint24(buf[v2EntryOffsetOriginalLength:], it.GetOriginalLength())

	//  11-13: packed length bits 0..23

	encodeBigEndianUint24(buf[v2EntryOffsetPackedLength:], it.GetPackedLength())

	//  14-15: pack ID (lower 16 bits)- index into Packs[]

	packBlobIndex := b.packID2Index[it.GetPackBlobID()]
	binary.BigEndian.PutUint16(buf[v2EntryOffsetPackBlobID:], uint16(packBlobIndex))

	//     16: format ID - index into Formats[] - 0 - present if not all formats are identical

	buf[v2EntryOffsetFormatID] = b.uniqueFormatInfo2Index[indexV2FormatInfoFromInfo(it)]

	//     17: pack ID - bits 16..23 - present if more than 2^16 packs are in a single index
	buf[v2EntryOffsetExtendedPackBlobID] = byte(packBlobIndex >> v2EntryExtendedPackBlobIDShift)

	//     18: high-order bits - present if any content length is greater than 2^24 == 16MiB
	//            original length bits 24..27  (4 hi bits)
	//            packed length bits 24..27    (4 lo bits)
	buf[v2EntryOffsetHighLengthBits] = byte(it.GetPackedLength()>>v2EntryHighLengthShift) | byte((it.GetOriginalLength()>>v2EntryHighLengthShift)<<v2EntryHighLengthBitsOriginalLengthShift)

	for i := b.entrySize; i < v2EntryMaxLength; i++ {
		if buf[i] != 0 {
			panic(fmt.Sprintf("encoding bug %x (entrySize=%v)", buf, b.entrySize))
		}
	}

	_, err := w.Write(buf[0:b.entrySize])

	return errors.Wrap(err, "error writing index value entry")
}

func openV2PackIndex(readerAt io.ReaderAt) (Index, error) {
	var header [v2IndexHeaderSize]byte

	if err := readAtAll(readerAt, header[:], 0); err != nil {
		return nil, errors.Wrap(err, "invalid header")
	}

	hi := v2HeaderInfo{
		version:       int(header[0]),
		keySize:       int(header[1]),
		entrySize:     int(binary.BigEndian.Uint16(header[2:4])),
		entryCount:    int(binary.BigEndian.Uint32(header[4:8])),
		packCount:     uint(binary.BigEndian.Uint32(header[8:12])),
		formatCount:   header[12],
		baseTimestamp: binary.BigEndian.Uint32(header[13:17]),
	}

	if hi.keySize <= 1 || hi.entrySize < v2EntryMinLength || hi.entrySize > v2EntryMaxLength || hi.entryCount < 0 || hi.formatCount > v2MaxFormatCount {
		return nil, errors.Errorf("invalid header")
	}

	hi.entryStride = int64(hi.keySize + hi.entrySize)
	if hi.entryStride > v2MaxEntrySize {
		return nil, errors.Errorf("invalid header - entry stride too big")
	}

	hi.entriesOffset = v2IndexHeaderSize
	hi.packsOffset = hi.entriesOffset + int64(hi.entryCount)*hi.entryStride
	hi.formatsOffset = hi.packsOffset + int64(hi.packCount*v2PackInfoSize)

	// pre-read formats section
	formatsBuf := make([]byte, int(hi.formatCount)*v2FormatInfoSize)
	if err := readAtAll(readerAt, formatsBuf, hi.formatsOffset); err != nil {
		return nil, errors.Errorf("unable to read formats section")
	}

	return &indexV2{
		hdr:      hi,
		readerAt: readerAt,
		formats:  parseFormatsBuffer(formatsBuf, int(hi.formatCount)),
	}, nil
}

func parseFormatsBuffer(formatsBuf []byte, cnt int) []indexV2FormatInfo {
	formats := make([]indexV2FormatInfo, cnt)

	for i := 0; i < cnt; i++ {
		f := formatsBuf[v2FormatInfoSize*i:]

		formats[i].compressionHeaderID = compression.HeaderID(binary.BigEndian.Uint32(f[v2FormatOffsetCompressionID:]))
		formats[i].formatVersion = f[v2FormatOffsetFormatVersion]
		formats[i].encryptionKeyID = f[v2FormatOffsetEncryptionKeyID]
	}

	return formats
}
