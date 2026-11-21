package cli

import "strings"

// CompareVersions compares numeric components, treating omitted ones as zero.
func CompareVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(left), len(right)); i++ {
		l, r := versionComponent(left, i), versionComponent(right, i)
		if len(l) != len(r) {
			return len(l) - len(r)
		}
		if cmp := strings.Compare(l, r); cmp != 0 {
			return cmp
		}
	}
	return 0
}

func versionComponent(parts []string, i int) string {
	if i >= len(parts) {
		return ""
	}
	return strings.TrimLeft(parts[i], "0")
}
