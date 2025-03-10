package content

import "github.com/kopia/kopia/repo/content/index"

type (
	// ID is an identifier of content in content-addressable storage.
	ID = index.ID

	// Info is an information about a single piece of content managed by Manager.
	Info = index.Info

	// InfoStruct is an implementation of Info based on a structure.
	InfoStruct = index.InfoStruct

	// IDRange represents a range of IDs.
	IDRange = index.IDRange
)

// ToInfoStruct converts the provided Info to *InfoStruct.
func ToInfoStruct(i Info) *InfoStruct {
	return index.ToInfoStruct(i)
}

// IDsFromStrings converts strings to IDs.
func IDsFromStrings(str []string) []ID {
	var result []ID

	for _, v := range str {
		result = append(result, ID(v))
	}

	return result
}

// IDsToStrings converts the IDs to strings.
func IDsToStrings(input []ID) []string {
	var result []string

	for _, v := range input {
		result = append(result, string(v))
	}

	return result
}
