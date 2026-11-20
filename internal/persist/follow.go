package persist

// Follower is a Snapshotter whose Restore reads the state another service's
// Restore put back, as DynamoDB Streams' records go back into the shards of
// DynamoDB's restored tables. Load restores the followed service, its leader,
// first. A flush saves the follower only right after it renamed its leader's
// snapshot into place, whether or not the follower changed, so the follower's
// file is never of a later moment than its leader's file on disk. When the
// leader is not saved (its backoff, a failure, or it is not persisted) the
// follower is not saved and stays dirty; when the follower is in its own
// backoff the leader is saved alone, and the follower's file is the older of
// the two until both are saved again.
type Follower interface {
	// Follows names the service this one follows.
	Follows() string
}

// Committer is a Snapshotter told when the snapshot it last took is on disk:
// a flush calls Committed once it has renamed every state file of that
// snapshot into place, before it saves the service's followers. A snapshot
// that fails, or whose rename does not finish, is not committed.
type Committer interface {
	Committed()
}

// follows is the service s follows, "" for none.
func follows(s Snapshotter) string {
	if f, ok := s.(Follower); ok {
		return f.Follows()
	}

	return ""
}

// afterLeaders orders list, keeping its order otherwise, so that each
// follower comes right after the service it follows when that service is in
// list too. ids returns an element's service name and the name it follows.
func afterLeaders[T any](list []T, ids func(T) (name, leader string)) []T {
	present := make(map[string]bool, len(list))

	for _, x := range list {
		name, _ := ids(x)
		present[name] = true
	}

	out := make([]T, 0, len(list))
	placed := make([]bool, len(list))

	var place func(i int)

	place = func(i int) {
		placed[i] = true
		out = append(out, list[i])
		name, _ := ids(list[i])

		for j, x := range list {
			if _, leader := ids(x); !placed[j] && leader == name {
				place(j)
			}
		}
	}

	for i, x := range list {
		if _, leader := ids(x); !placed[i] && (leader == "" || !present[leader]) {
			place(i)
		}
	}

	// A cycle of followers has no leader to go after: it keeps its order.
	for i := range list {
		if !placed[i] {
			place(i)
		}
	}

	return out
}
