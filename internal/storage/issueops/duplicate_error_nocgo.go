//go:build !embeddeddolt

package issueops

func isEmbeddedDuplicateError(error) bool {
	return false
}
