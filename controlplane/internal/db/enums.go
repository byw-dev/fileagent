package db

// Valid reports whether e is one of the defined file_status enum values. It lets
// callers reject an unknown status up front rather than failing later when the
// value is cast to the file_status enum in SQL.
func (e FileStatus) Valid() bool {
	switch e {
	case FileStatusUploading, FileStatusCompleted, FileStatusFailed, FileStatusDeleted:
		return true
	default:
		return false
	}
}
