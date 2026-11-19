// Package blob holds the bodies a service streams in and out, such as S3's
// objects and parts, under ids the store picks. Store is the contract; Ephemeral
// keeps bodies in memory and in unlinked temporary files, for a service that is
// not persisted. A persisted service keeps its bodies through persist.Assets.
package blob
