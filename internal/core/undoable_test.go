package core

import "testing"

func TestArchiveIsNotOfferedAsUndoable(t *testing.T) {
	before := []byte(`{"id":7,"archived_at":null}`)
	after := []byte(`{"id":7,"archived_at":123}`)
	if Undoable("proposition", "archive", before, after, false) {
		t.Fatal("archive was offered as undoable even though archived propositions refuse undo writes")
	}
	if !Undoable("proposition", "restore", after, before, false) {
		t.Fatal("restore was not offered as undoable once the proposition was writable again")
	}
}
