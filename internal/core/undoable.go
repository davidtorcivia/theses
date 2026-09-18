package core

// Undoable reports whether an activity row stands a chance of being undone. It
// is the same rule Undo applies before it opens its transaction, so a panel can
// draw the control only where it means something. The checks Undo makes inside
// the transaction, that the row is still there and its ordering key still free,
// are not repeated here: those can only be answered under the lock, and a
// refusal there is a message rather than a missing button.
func Undoable(entity, action string, before, after []byte, undone bool) bool {
	spec, ok := undoable[entity]
	if !ok || undone || len(before) == 0 || len(after) == 0 ||
		action == "create" || action == "undo" ||
		(action == "delete" && !spec.tombstone) {
		return false
	}
	was, err := decode(string(before))
	if err != nil {
		return false
	}
	now, err := decode(string(after))
	if err != nil {
		return false
	}
	return !unchanged(spec.cols, was, now)
}
