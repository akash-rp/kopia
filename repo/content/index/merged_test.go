package index

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/kopia/kopia/repo/blob"
)

func TestMerged(t *testing.T) {
	i1, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 11},
		&InfoStruct{ContentID: mustParseID(t, "ddeeff"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 111},
		&InfoStruct{ContentID: mustParseID(t, "z010203"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 111},
		&InfoStruct{ContentID: mustParseID(t, "de1e1e"), TimestampSeconds: 4, PackBlobID: "xx", PackOffset: 111},
	)
	require.NoError(t, err)

	i2, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 3, PackBlobID: "yy", PackOffset: 33},
		&InfoStruct{ContentID: mustParseID(t, "xaabbcc"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 111},
		&InfoStruct{ContentID: mustParseID(t, "de1e1e"), TimestampSeconds: 4, PackBlobID: "xx", PackOffset: 222, Deleted: true},
	)
	require.NoError(t, err)

	i3, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 2, PackBlobID: "zz", PackOffset: 22},
		&InfoStruct{ContentID: mustParseID(t, "ddeeff"), TimestampSeconds: 1, PackBlobID: "zz", PackOffset: 222},
		&InfoStruct{ContentID: mustParseID(t, "k010203"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 111},
		&InfoStruct{ContentID: mustParseID(t, "k020304"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 111},
	)
	require.NoError(t, err)

	m := Merged{i1, i2, i3}

	require.Equal(t, m.ApproximateCount(), 11)

	i, err := m.GetInfo(mustParseID(t, "aabbcc"))
	require.NoError(t, err)
	require.NotNil(t, i)

	require.Equal(t, uint32(33), i.GetPackOffset())

	require.NoError(t, m.Iterate(AllIDs, func(i Info) error {
		if i.GetContentID() == mustParseID(t, "de1e1e") {
			if i.GetDeleted() {
				t.Errorf("iteration preferred deleted content over non-deleted")
			}
		}
		return nil
	}))

	fmt.Println("=========== START")

	// error is propagated.
	someErr := errors.Errorf("some error")
	require.ErrorIs(t, m.Iterate(AllIDs, func(i Info) error {
		if i.GetContentID() == mustParseID(t, "aabbcc") {
			return someErr
		}

		return nil
	}), someErr)

	fmt.Println("=========== END")

	// empty merged index does not invoke callback during iteration.
	require.NoError(t, Merged{}.Iterate(AllIDs, func(i Info) error {
		return someErr
	}))

	i, err = m.GetInfo(mustParseID(t, "de1e1e"))
	require.NoError(t, err)
	require.False(t, i.GetDeleted())

	cases := []struct {
		r IDRange

		wantIDs []ID
	}{
		{
			r: AllIDs,
			wantIDs: []ID{
				mustParseID(t, "aabbcc"),
				mustParseID(t, "ddeeff"),
				mustParseID(t, "de1e1e"),
				mustParseID(t, "k010203"),
				mustParseID(t, "k020304"),
				mustParseID(t, "xaabbcc"),
				mustParseID(t, "z010203"),
			},
		},
		{
			r: AllNonPrefixedIDs,
			wantIDs: []ID{
				mustParseID(t, "aabbcc"),
				mustParseID(t, "ddeeff"),
				mustParseID(t, "de1e1e"),
			},
		},
		{
			r: AllPrefixedIDs,
			wantIDs: []ID{
				mustParseID(t, "k010203"),
				mustParseID(t, "k020304"),
				mustParseID(t, "xaabbcc"),
				mustParseID(t, "z010203"),
			},
		},
		{
			r: IDRange{"a", "e"},
			wantIDs: []ID{
				mustParseID(t, "aabbcc"),
				mustParseID(t, "ddeeff"),
				mustParseID(t, "de1e1e"),
			},
		},
		{
			r: PrefixRange("dd"),
			wantIDs: []ID{
				mustParseID(t, "ddeeff"),
			},
		},
		{
			r: IDRange{"dd", "df"},
			wantIDs: []ID{
				mustParseID(t, "ddeeff"),
				mustParseID(t, "de1e1e"),
			},
		},
	}

	for _, tc := range cases {
		inOrder := iterateIDRange(t, m, tc.r)
		if !reflect.DeepEqual(inOrder, tc.wantIDs) {
			t.Errorf("unexpected items in order: %v, wanted %v", inOrder, tc.wantIDs)
		}
	}

	if err := m.Close(); err != nil {
		t.Errorf("unexpected error in Close(): %v", err)
	}
}

type failingIndex struct {
	Index
	err error
}

func (i failingIndex) GetInfo(contentID ID) (Info, error) {
	return nil, i.err
}

func TestMergedGetInfoError(t *testing.T) {
	someError := errors.Errorf("some error")

	m := Merged{failingIndex{nil, someError}}

	info, err := m.GetInfo(mustParseID(t, "xabcdef"))
	require.ErrorIs(t, err, someError)
	require.Nil(t, info)
}

func TestMergedIndexIsConsistent(t *testing.T) {
	i1, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 11},
		&InfoStruct{ContentID: mustParseID(t, "bbccdd"), TimestampSeconds: 1, PackBlobID: "xx", PackOffset: 11},
		&InfoStruct{ContentID: mustParseID(t, "ccddee"), TimestampSeconds: 1, PackBlobID: "ff", PackOffset: 11, Deleted: true},
	)
	require.NoError(t, err)

	i2, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 1, PackBlobID: "yy", PackOffset: 33},
		&InfoStruct{ContentID: mustParseID(t, "bbccdd"), TimestampSeconds: 1, PackBlobID: "yy", PackOffset: 11, Deleted: true},
		&InfoStruct{ContentID: mustParseID(t, "ccddee"), TimestampSeconds: 1, PackBlobID: "gg", PackOffset: 11, Deleted: true},
	)
	require.NoError(t, err)

	i3, err := indexWithItems(
		&InfoStruct{ContentID: mustParseID(t, "aabbcc"), TimestampSeconds: 1, PackBlobID: "zz", PackOffset: 22},
		&InfoStruct{ContentID: mustParseID(t, "bbccdd"), TimestampSeconds: 1, PackBlobID: "zz", PackOffset: 11, Deleted: true},
		&InfoStruct{ContentID: mustParseID(t, "ccddee"), TimestampSeconds: 1, PackBlobID: "hh", PackOffset: 11, Deleted: true},
	)
	require.NoError(t, err)

	cases := []Merged{
		{i1, i2, i3},
		{i1, i3, i2},
		{i2, i1, i3},
		{i2, i3, i1},
		{i3, i1, i2},
		{i3, i2, i1},
	}

	for _, m := range cases {
		i, err := m.GetInfo(mustParseID(t, "aabbcc"))
		if err != nil || i == nil {
			t.Fatalf("unable to get info: %v", err)
		}

		// all things being equal, highest pack blob ID wins
		require.Equal(t, blob.ID("zz"), i.GetPackBlobID())

		i, err = m.GetInfo(mustParseID(t, "bbccdd"))
		if err != nil || i == nil {
			t.Fatalf("unable to get info: %v", err)
		}

		// given identical timestamps, non-deleted wins.
		require.Equal(t, blob.ID("xx"), i.GetPackBlobID())

		i, err = m.GetInfo(mustParseID(t, "ccddee"))
		if err != nil || i == nil {
			t.Fatalf("unable to get info: %v", err)
		}

		// given identical timestamps and all deleted, highest pack blob ID wins.
		require.Equal(t, blob.ID("hh"), i.GetPackBlobID())
	}
}

func iterateIDRange(t *testing.T, m Index, r IDRange) []ID {
	t.Helper()

	var inOrder []ID

	require.NoError(t, m.Iterate(r, func(i Info) error {
		inOrder = append(inOrder, i.GetContentID())
		return nil
	}))

	return inOrder
}

func indexWithItems(items ...Info) (Index, error) {
	b := make(Builder)

	for _, it := range items {
		b.Add(it)
	}

	var buf bytes.Buffer
	if err := b.Build(&buf, Version2); err != nil {
		return nil, errors.Wrap(err, "build error")
	}

	return Open(buf.Bytes(), nil, fakeEncryptionOverhead)
}
