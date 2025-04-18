package util

import (
	"slices"

	"k8s.io/utils/set"
)

func DiffStringSlice(slice1, slice2 []string) []string {
	var diff []string

	// Loop two times, first to find slice1 strings not in slice2,
	// second loop to find slice2 strings not in slice1
	for i := range 2 {
		for _, s1 := range slice1 {
			found := slices.Contains(slice2, s1)
			// String not found. We add it to return slice
			if !found {
				diff = append(diff, s1)
			}
		}
		// Swap the slices, only if it was the first loop
		if i == 0 {
			slice1, slice2 = slice2, slice1
		}
	}
	return diff
}

func UnionStringSlice(slices ...[]string) []string {
	union := set.New[string]()
	for _, s := range slices {
		union.Insert(s...)
	}
	return union.UnsortedList()
}

// IsStringsOverlap check if two string slices are overlapped
func IsStringsOverlap(a, b []string) bool {
	for _, sa := range a {
		if slices.Contains(b, sa) {
			return true
		}
	}
	return false
}

func RemoveString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return result
}
