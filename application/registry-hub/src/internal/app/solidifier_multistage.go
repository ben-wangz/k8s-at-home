package app

type stagedManifest struct {
	ref        contentRef
	blobs      []contentRef
	children   []contentRef
	scopeID    string
	sessionID  string
	scopeBytes int64
}

type stagedGraph struct {
	root       contentRef
	manifests  []*stagedManifest
	byManifest map[string]*stagedManifest
	blobs      map[string]contentRef
	active     map[string]bool
	objects    int
	bytes      int64
}
