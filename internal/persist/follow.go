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
// the two until both are saved again. A follower's leader follows no service,
// and no service follows a follower: [Engine.Register] refuses a chain.
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

// Leader is a Snapshotter told whether a [Follower] is saved from its
// snapshot, so it keeps what its followers write only when one will. A flush
// calls Followed before each Snapshot: true when it saves a follower right
// after this service, false when it saves the service alone (the follower is
// not persisted, is unsafe, or is in its own backoff). A flush that told it
// true tells it Followed(false) once the saves of both have ended, whatever
// happened to either, and before another flush may take them, so nothing it
// kept for its follower outlives them.
type Leader interface {
	Followed(saved bool)
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
