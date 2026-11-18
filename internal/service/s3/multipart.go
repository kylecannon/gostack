package s3

import "time"

// MultipartUpload is an upload in progress. Its parts are blobs until it
// completes into one.
type MultipartUpload struct {
	ID        string
	Key       string
	Initiated time.Time

	Metadata           map[string]string
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Expires            string

	StorageClass         string
	ServerSideEncryption string
	ChecksumAlgorithm    string
	ChecksumType         string

	// Parts maps a part number to its latest upload.
	Parts map[int32]*Part
}

// Part is one uploaded part of a multipart upload.
type Part struct {
	Number        int32
	BlobID        string
	Size          int64
	ETag          string
	LastModified  time.Time
	ChecksumValue string
}
