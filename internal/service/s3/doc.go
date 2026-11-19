// Package s3 implements the S3 API behind the generated dispatcher in
// internal/api/s3: buckets and their configuration, objects and their versions,
// and the bodies behind them, which live in a blob.Store and are streamed in and
// out, never held whole.
package s3
