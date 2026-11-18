package s3

import "slices"

// listedKeys is a sorted slice of keys as a keySource, which lets a test hand
// the walks the keys it wants without a bucket.
type listedKeys []listed

func (s listedKeys) len() int { return len(s) }

func (s listedKeys) at(i int) listed { return s[i] }

func (s listedKeys) seek(key string) int {
	i, _ := slices.BinarySearchFunc(s, key, func(k listed, key string) int {
		switch {
		case k.key < key:
			return -1
		case k.key > key:
			return 1
		default:
			return 0
		}
	})

	return i
}

// walkKeys is walkKeysFrom over keys.
func walkKeys(keys []listed, w walk) listPage { return walkKeysFrom(listedKeys(keys), w) }

// walkVersions is walkVersionsFrom over keys.
func walkVersions(keys []listed, w walk) versionPage { return walkVersionsFrom(listedKeys(keys), w) }
