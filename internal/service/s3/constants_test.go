package s3_test

// Names the object tests share.
const (
	headerMetaColor         = "X-Amz-Meta-Color"
	headerMetaName          = "X-Amz-Meta-Name"
	headerStorageClass      = "X-Amz-Storage-Class"
	headerWebsiteRedirect   = "X-Amz-Website-Redirect-Location"
	headerTagging           = "X-Amz-Tagging"
	headerRange             = "Range"
	headerIfUnmodifiedSince = "If-Unmodified-Since"
	headerIfModifiedSince   = "If-Modified-Since"
	headerMetadataDirective = "X-Amz-Metadata-Directive"

	colorBlue            = "blue"
	headerChecksumMode   = "X-Amz-Checksum-Mode"
	checksumModeOn       = "ENABLED"
	nameCRC32            = "CRC32"
	benchListen          = "0.0.0.0:4566"
	headerAcceptEncoding = "Accept-Encoding"
	readChecksumCRC32    = "X-Amz-Checksum-Crc32"
	readChecksumType     = "X-Amz-Checksum-Type"
	readPartsCount       = "X-Amz-Mp-Parts-Count"

	contentTypePlain = "text/plain"
	encodingIdentity = "identity"
	valueReplace     = "REPLACE"
	etagOther        = `"other"`
	etagX            = `"x"`
	targetDelete     = "/bucket?delete"
)
