// The forge's store: git objects, packs, trees, refs and the GitHub resources
// (repositories, issues, pull requests) that all four surfaces share.
//
// Everything here is in-memory and deterministic: server-built commits use a
// fixed identity and timestamp, so two runs that take the same requests build
// the same object ids. No wall clock, no randomness, no git binary, no network.
//
// This is a fresh Go implementation of the same behaviour the TypeScript
// forge had: the object model, pack codec, reachability and the resource
// model are ported one function at a time, with the same ids, the same
// errors and the same deterministic timestamps.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"
const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const fixedEpochSec = 1735689600 // 2025-01-01T00:00:00Z
const fixedISO = "2025-01-01T00:00:00Z"

// ObjType is a git object type.
type ObjType string

const (
	objCommit ObjType = "commit"
	objTree   ObjType = "tree"
	objBlob   ObjType = "blob"
	objTag    ObjType = "tag"
)

// GitObject is one stored git object. commit/tree are lazily parsed caches;
// objects are immutable once stored and every access runs under the Forge
// mutex, so the caches are sound.
type GitObject struct {
	Type       ObjType
	Data       []byte
	commit     *CommitView
	tree       []TreeEntry
	treeParsed bool
}

var typeCode = map[ObjType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]ObjType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func hashObject(typ ObjType, data []byte) string {
	head := []byte(fmt.Sprintf("%s %d\x00", typ, len(data)))
	h := sha1.New()
	h.Write(head)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------------------------
// object parsing and building
// ---------------------------------------------------------------------------------------------

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], 0x20)
		if sp < 0 {
			return nil, errors.New("bad tree entry")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0x00)
		if nul < 0 {
			return nil, errors.New("bad tree entry")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("bad tree entry")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

func (o *GitObject) parsedTree() []TreeEntry {
	if o.treeParsed {
		return o.tree
	}
	entries, err := parseTree(o.Data)
	if err != nil {
		entries = nil
	}
	o.tree = entries
	o.treeParsed = true
	return o.tree
}

func compareTreeNames(a, b TreeEntry) int {
	left := a.Name
	if a.Mode == "40000" {
		left = a.Name + "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right = b.Name + "/"
	}
	return bytes.Compare([]byte(left), []byte(right))
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// CommitView is a parsed commit's headers.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func (o *GitObject) parsedCommit() *CommitView {
	if o.commit != nil {
		return o.commit
	}
	o.commit = parseCommitUncached(o.Data)
	return o.commit
}

func parseCommitUncached(data []byte) *CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	view := &CommitView{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		key, value := line, ""
		if sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	view.Message = message
	return view
}

func buildCommit(tree string, parents []string, message, author, committer string, timestamp int) []byte {
	ts := timestamp
	if ts == 0 {
		ts = fixedEpochSec
	}
	who := author
	if who == "" {
		who = viewerName + " <" + viewerEmail + ">"
	}
	who += fmt.Sprintf(" %d +0000", ts)
	comm := committer
	if comm == "" {
		comm = viewerName + " <" + viewerEmail + ">"
	}
	comm += fmt.Sprintf(" %d +0000", ts)
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n", who, comm)
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	b.WriteString(msg)
	return []byte(b.String())
}

// TagView is a parsed annotated tag's headers.
type TagView struct {
	Object  string
	Type    ObjType
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) *TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		if sp < 0 {
			continue
		}
		headers[line[:sp]] = line[sp+1:]
	}
	typ := headers["type"]
	var t ObjType
	switch typ {
	case "commit", "tree", "blob", "tag":
		t = ObjType(typ)
	default:
		t = objCommit
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	return &TagView{Object: headers["object"], Type: t, Tag: headers["tag"], Tagger: headers["tagger"], Message: message}
}

// ---------------------------------------------------------------------------------------------
// reachability, ancestry, trees
// ---------------------------------------------------------------------------------------------

// ObjectStore maps oid -> object.
type ObjectStore map[string]*GitObject

func collectReachable(store ObjectStore, roots []string) (map[string]bool, error) {
	seen := map[string]bool{}
	var stack []string
	for _, r := range roots {
		if isOID(r) {
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := store[oid]
		if obj == nil {
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch obj.Type {
		case objCommit:
			c := obj.parsedCommit()
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case objTag:
			stack = append(stack, parseTag(obj.Data).Object)
		case objTree:
			for _, e := range obj.parsedTree() {
				if e.Mode != "160000" {
					stack = append(stack, e.OID)
				}
			}
		}
	}
	return seen, nil
}

// isAncestor reports whether ancestor is reachable from tip by following parents (or equal).
func isAncestor(store ObjectStore, ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := store[oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		stack = append(stack, obj.parsedCommit().Parents...)
	}
	return false
}

// listCommits returns the first-parent chain (plus merge parents, breadth-first) from a tip, newest first.
func listCommits(store ObjectStore, tip string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := store[oid]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		out = append(out, oid)
		queue = append(queue, obj.parsedCommit().Parents...)
	}
	return out
}

type fileAt struct {
	OID  string
	Data []byte
}

// readFileAt walks a tree to path; returns nil when the path is not a blob.
func readFileAt(store ObjectStore, rootOID, path string) *fileAt {
	parts := splitPathParts(path)
	oid := rootOID
	for i, part := range parts {
		obj := store[oid]
		if obj == nil || obj.Type != objTree {
			return nil
		}
		entries := obj.parsedTree()
		var entry *TreeEntry
		for j := range entries {
			if entries[j].Name == part {
				entry = &entries[j]
				break
			}
		}
		if entry == nil {
			return nil
		}
		if i == len(parts)-1 {
			if entry.Mode == "40000" {
				return nil
			}
			blob := store[entry.OID]
			if blob == nil || blob.Type != objBlob {
				return nil
			}
			return &fileAt{OID: entry.OID, Data: blob.Data}
		}
		if entry.Mode != "40000" {
			return nil
		}
		oid = entry.OID
	}
	return nil
}

func splitPathParts(path string) []string {
	var out []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// listDirAt lists a directory's entries at path ("" for the root); nil when not a tree.
func listDirAt(store ObjectStore, rootOID, path string) []TreeEntry {
	oid := rootOID
	for _, part := range splitPathParts(path) {
		obj := store[oid]
		if obj == nil || obj.Type != objTree {
			return nil
		}
		var next string
		found := false
		for _, e := range obj.parsedTree() {
			if e.Name == part && e.Mode == "40000" {
				next = e.OID
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		oid = next
	}
	obj := store[oid]
	if obj == nil || obj.Type != objTree {
		return nil
	}
	return obj.parsedTree()
}

// updateTree returns a new root tree oid with path set to blobOID (or removed when blobOID is "").
// Missing intermediate trees are created; empty directories are pruned.
func updateTree(store ObjectStore, rootOID, path, blobOID string) string {
	parts := splitPathParts(path)
	var put func(oid string, hasOID bool, depth int) (string, bool)
	put = func(oid string, hasOID bool, depth int) (string, bool) {
		var entries []TreeEntry
		if hasOID {
			if obj := store[oid]; obj != nil {
				entries = append([]TreeEntry(nil), obj.parsedTree()...)
			}
		}
		name := parts[depth]
		if depth == len(parts)-1 {
			idx := -1
			for i, e := range entries {
				if e.Name == name {
					idx = i
					break
				}
			}
			if blobOID == "" {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "100644", Name: name, OID: blobOID}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
		} else {
			idx := -1
			for i, e := range entries {
				if e.Name == name {
					idx = i
					break
				}
			}
			var childOID string
			var childHas bool
			if idx >= 0 && entries[idx].Mode == "40000" {
				childOID, childHas = entries[idx].OID, true
			}
			next, ok := put(childOID, childHas, depth+1)
			if !ok {
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else if idx >= 0 {
				entries[idx] = TreeEntry{Mode: "40000", Name: name, OID: next}
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: next})
			}
		}
		if len(entries) == 0 {
			return "", false
		}
		data := buildTree(entries)
		newOID := hashObject(objTree, data)
		if _, ok := store[newOID]; !ok {
			store[newOID] = &GitObject{Type: objTree, Data: data}
		}
		return newOID, true
	}
	oid, ok := put(rootOID, rootOID != "", 0)
	if !ok {
		data := buildTree(nil)
		emptyOID := hashObject(objTree, data)
		if _, exists := store[emptyOID]; !exists {
			store[emptyOID] = &GitObject{Type: objTree, Data: data}
		}
		return emptyOID
	}
	return oid
}

// FileChange is one file-level diff entry.
type FileChange struct {
	Path      string
	Status    string // added | modified | removed
	OldOID    string
	NewOID    string
	Additions int
	Deletions int
}

func lineDiff(oldText, newText string) (additions, deletions int) {
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	counts := map[string]int{}
	for _, l := range oldLines {
		counts[l]++
	}
	for _, l := range newLines {
		if counts[l] > 0 {
			counts[l]--
		} else {
			additions++
		}
	}
	for _, c := range counts {
		deletions += c
	}
	return additions, deletions
}

func flattenTree(store ObjectStore, rootOID, prefix string, out map[string]string) {
	obj := store[rootOID]
	if obj == nil || obj.Type != objTree {
		return
	}
	for _, e := range obj.parsedTree() {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			flattenTree(store, e.OID, path, out)
		} else if e.Mode != "160000" {
			out[path] = e.OID
		}
	}
}

// diffTrees is the file-level diff between two root trees ("" means no tree).
func diffTrees(store ObjectStore, oldRoot, newRoot string) []FileChange {
	oldFiles := map[string]string{}
	newFiles := map[string]string{}
	if oldRoot != "" {
		flattenTree(store, oldRoot, "", oldFiles)
	}
	if newRoot != "" {
		flattenTree(store, newRoot, "", newFiles)
	}
	paths := map[string]bool{}
	for p := range oldFiles {
		paths[p] = true
	}
	for p := range newFiles {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var changes []FileChange
	for _, path := range sorted {
		oldOID, hasOld := oldFiles[path]
		newOID, hasNew := newFiles[path]
		if hasOld && hasNew && oldOID == newOID {
			continue
		}
		if !hasOld && hasNew {
			data := []byte{}
			if obj := store[newOID]; obj != nil {
				data = obj.Data
			}
			lines := 0
			if len(data) > 0 {
				lines = len(strings.Split(string(data), "\n"))
			}
			changes = append(changes, FileChange{Path: path, Status: "added", NewOID: newOID, Additions: lines})
		} else if hasOld && !hasNew {
			data := []byte{}
			if obj := store[oldOID]; obj != nil {
				data = obj.Data
			}
			lines := 0
			if len(data) > 0 {
				lines = len(strings.Split(string(data), "\n"))
			}
			changes = append(changes, FileChange{Path: path, Status: "removed", OldOID: oldOID, Deletions: lines})
		} else {
			oldData := []byte{}
			if obj := store[oldOID]; obj != nil {
				oldData = obj.Data
			}
			newData := []byte{}
			if obj := store[newOID]; obj != nil {
				newData = obj.Data
			}
			a, d := lineDiff(string(oldData), string(newData))
			changes = append(changes, FileChange{Path: path, Status: "modified", OldOID: oldOID, NewOID: newOID, Additions: a, Deletions: d})
		}
	}
	return changes
}

// mergeBase is the first common ancestor of two commits by breadth-first search.
func mergeBase(store ObjectStore, a, b string) string {
	ancestorsA := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		oid := queueA[0]
		queueA = queueA[1:]
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		if obj := store[oid]; obj != nil && obj.Type == objCommit {
			queueA = append(queueA, obj.parsedCommit().Parents...)
		}
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		oid := queueB[0]
		queueB = queueB[1:]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ancestorsA[oid] {
			return oid
		}
		if obj := store[oid]; obj != nil && obj.Type == objCommit {
			queueB = append(queueB, obj.parsedCommit().Parents...)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// packfiles
// ---------------------------------------------------------------------------------------------

func readVarint(buf []byte, start int) (size int, next int, first int, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry size varint is truncated")
	}
	b := buf[start]
	first = int(b)
	size = int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, off, first, nil
}

func readOfsBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta base offset is truncated")
	}
	b := buf[start]
	off := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[off]
		off++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, off, nil
}

// oneByteReader hands out at most one byte per Read. Go's flate reader
// buffers input it has read but not yet consumed, so with a normal reader
// the stream's exact end cannot be recovered from the underlying position;
// with one-byte reads the bytes handed out are exactly the bytes consumed.
type oneByteReader struct {
	buf   []byte
	pos   int
	count int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.buf[r.pos]
	r.pos++
	r.count++
	return 1, nil
}

// inflateAt inflates one zlib stream starting at offset and returns the data
// plus the offset just past the stream.
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	r := &oneByteReader{buf: buf[offset:]}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return out, offset + r.count, nil
}

// applyDelta applies a git delta to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[off]
			off++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOffset += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if off+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:], delta[off:off+literal])
			off += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte
	baseOffset int
	baseOID    string
}

// readPack reads a packfile into objects (deltas resolved). Throws on any corruption.
func readPack(pack []byte) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	if sha1Hex(pack[:trailerStart]) != trailer {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	var raw []*rawPackEntry
	byOffset := map[int]*rawPackEntry{}
	off := 12
	for i := 0; i < count; i++ {
		entryOffset := off
		size, next, first, err := readVarint(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		typeCode := (first >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			base, next, err := readOfsBase(pack, off)
			if err != nil {
				return nil, err
			}
			off = next
			if base <= 0 || base > entryOffset {
				return nil, errors.New("ofs-delta names an invalid base offset")
			}
			entry.baseOffset = entryOffset - base
		} else if typeCode == 7 {
			if off+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if codeType[typeCode] == "" {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], off)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		off = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if off != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-off)
	}

	objects := map[string]*GitObject{}
	resolvedByOID := map[string]*GitObject{}
	plainByOID := map[string]*rawPackEntry{}
	for _, entry := range raw {
		if entry.typeCode != 6 && entry.typeCode != 7 {
			plainByOID[hashObject(codeType[entry.typeCode], entry.data)] = entry
		}
	}
	resolving := map[int]bool{}
	var resolve func(entry *rawPackEntry) (*GitObject, error)
	resolve = func(entry *rawPackEntry) (*GitObject, error) {
		if resolving[entry.offset] {
			return nil, fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		defer delete(resolving, entry.offset)
		var typ ObjType
		var data []byte
		if entry.typeCode == 6 {
			baseEntry := byOffset[entry.baseOffset]
			if baseEntry == nil {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			typ = base.Type
			data, err = applyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOID
			if direct, ok := resolvedByOID[wanted]; ok {
				typ = direct.Type
				var err error
				data, err = applyDelta(direct.Data, entry.data)
				if err != nil {
					return nil, err
				}
			} else if baseEntry, ok := plainByOID[wanted]; ok {
				base, err := resolve(baseEntry)
				if err != nil {
					return nil, err
				}
				typ = base.Type
				data, err = applyDelta(base.Data, entry.data)
				if err != nil {
					return nil, err
				}
			} else {
				var found *GitObject
				for _, candidate := range raw {
					if candidate.offset == entry.offset {
						continue
					}
					r, err := resolve(candidate)
					if err != nil {
						continue
					}
					if hashObject(r.Type, r.Data) == wanted {
						found = r
						break
					}
				}
				if found == nil {
					return nil, fmt.Errorf("ref-delta base %s is not in the pack", wanted)
				}
				typ = found.Type
				var err error
				data, err = applyDelta(found.Data, entry.data)
				if err != nil {
					return nil, err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		obj := &GitObject{Type: typ, Data: data}
		resolvedByOID[oid] = obj
		return obj, nil
	}
	for _, entry := range raw {
		r, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		oid := hashObject(r.Type, r.Data)
		if _, ok := objects[oid]; !ok {
			objects[oid] = r
		}
	}
	return objects, nil
}

func writePackObjectHeader(typ ObjType, size int) []byte {
	code := typeCode[typ]
	var bytesOut []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		bytesOut = append(bytesOut, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	bytesOut = append(bytesOut, byte(first))
	return bytesOut
}

// writePack builds a packfile from plain objects (no deltas).
// Level 1 is far faster than the default and still a valid zlib stream;
// pack bytes are not scored, only their integrity, so trade size for speed.
func writePack(objects []*GitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	buf.Write(header)
	for _, object := range objects {
		buf.Write(writePackObjectHeader(object.Type, len(object.Data)))
		zw, _ := zlib.NewWriterLevel(&buf, 1)
		zw.Write(object.Data)
		zw.Close()
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

// ---------------------------------------------------------------------------------------------
// forge resources
// ---------------------------------------------------------------------------------------------

// IssueComment is one comment on an issue or pull request.
type IssueComment struct {
	ID   int
	Body string
}

// Issue is one issue in a repository.
type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Author    string
	Labels    []string
	Comments  []IssueComment
	CreatedAt string
	UpdatedAt string
	ClosedAt  string
}

// PullRequest is one pull request. State is 'open' or 'closed'; a merged
// pull request has State 'closed' and Merged true.
type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	Author         string
	HeadBranch     string
	BaseBranch     string
	HeadSha        string
	BaseSha        string
	Labels         []string
	Comments       []IssueComment
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       string
	MergedAt       string
	Merged         bool
	MergeCommitSha string
}

// Label is one repository label.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Repo is one repository's complete state.
type Repo struct {
	ID             int
	Name           string
	Owner          string
	Description    string
	Private        bool
	DefaultBranch  string
	Refs           map[string]string
	refsOrder      []string
	Objects        ObjectStore
	Labels         map[string]*Label
	labelsOrder    []string
	Issues         map[int]*Issue
	Pulls          map[int]*PullRequest
	NextNumber     int
	NextCommentID  int
	NextRepoID     int // unused per-repo, kept for parity
	Verified       map[string]bool
	PackCache      map[string][]byte
	AdV0Cache      map[string][]byte
	IssueJSONCache map[int][]byte
}

func newRepo(id int, name, owner, description string, private bool) *Repo {
	return &Repo{
		ID:             id,
		Name:           name,
		Owner:          owner,
		Description:    description,
		Private:        private,
		DefaultBranch:  "main",
		Refs:           map[string]string{},
		Objects:        ObjectStore{},
		Labels:         map[string]*Label{},
		Issues:         map[int]*Issue{},
		Pulls:          map[int]*PullRequest{},
		NextNumber:     1,
		NextCommentID:  1,
		Verified:       map[string]bool{},
		PackCache:      map[string][]byte{},
		AdV0Cache:      map[string][]byte{},
		IssueJSONCache: map[int][]byte{},
	}
}

func (r *Repo) setRef(name, oid string) {
	if _, ok := r.Refs[name]; !ok {
		r.refsOrder = append(r.refsOrder, name)
	}
	r.Refs[name] = oid
	// The v0 advertisement cache is keyed by the full ref state, so a ref
	// change self-invalidates; nothing to clear here.
}

func (r *Repo) deleteRef(name string) {
	if _, ok := r.Refs[name]; !ok {
		return
	}
	delete(r.Refs, name)
	for i, n := range r.refsOrder {
		if n == name {
			r.refsOrder = append(r.refsOrder[:i], r.refsOrder[i+1:]...)
			break
		}
	}
	// The v0 advertisement cache is keyed by the full ref state; no clearing needed.
}

func (r *Repo) headTip() string {
	if oid, ok := r.Refs["refs/heads/main"]; ok {
		return oid
	}
	if oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		return oid
	}
	return zeroOID
}

func (r *Repo) headRefName() string {
	if _, ok := r.Refs["refs/heads/main"]; ok {
		return "main"
	}
	return r.DefaultBranch
}

func sortedRefNames(r *Repo) []string {
	names := append([]string(nil), r.refsOrder...)
	sort.Strings(names)
	return names
}

// Forge holds every repository.
type Forge struct {
	Repos       map[string]*Repo
	reposOrder  []*Repo
	nextRepoID  int
	nextIssueID int
	nextPullID  int
	seqCounter  int
	globalCache map[string]string
	mu          sync.Mutex
}

func newForge() *Forge {
	return &Forge{
		Repos:       map[string]*Repo{},
		nextRepoID:  1,
		nextIssueID: 1,
		nextPullID:  1,
		globalCache: map[string]string{},
	}
}

func (f *Forge) lock()   { f.mu.Lock() }
func (f *Forge) unlock() { f.mu.Unlock() }

func (f *Forge) requestID() string {
	f.seqCounter++
	return fmt.Sprintf("req-%08x", f.seqCounter)
}

// globalID encodes a namespaced global id, with the same cache eviction as the reference.
func (f *Forge) globalID(kind string, parts ...string) string {
	raw := kind + ":" + strings.Join(parts, ":")
	if encoded, ok := f.globalCache[raw]; ok {
		return encoded
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	if len(f.globalCache) >= 4096 {
		f.globalCache = map[string]string{}
	}
	f.globalCache[raw] = encoded
	return encoded
}

// parseGlobalID decodes a base64 global id into kind + parts; nil when malformed.
// Node's base64 decoder is lenient (it skips characters outside the alphabet),
// so decode leniently here too.
func parseGlobalID(value string) (kind string, parts []string, ok bool) {
	raw, err := lenientBase64Decode(value)
	if err != nil {
		return "", nil, false
	}
	fields := strings.Split(string(raw), ":")
	if len(fields) < 1 || fields[0] == "" || len(fields) < 2 {
		return "", nil, false
	}
	return fields[0], fields[1:], true
}

// lenientBase64Decode mirrors Node's Buffer.from(s, 'base64'): characters
// outside the base64 alphabet are ignored, missing padding is tolerated.
func lenientBase64Decode(s string) ([]byte, error) {
	var filtered []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	for len(filtered)%4 != 0 {
		filtered = append(filtered, '=')
	}
	return base64.StdEncoding.DecodeString(string(filtered))
}

func (f *Forge) listViewerRepos() []*Repo {
	var out []*Repo
	for _, r := range f.reposOrder {
		if r.Owner == viewerLogin {
			out = append(out, r)
		}
	}
	return out
}

func (f *Forge) createRepo(name, description string, private bool) *Repo {
	repo := newRepo(f.nextRepoID, name, viewerLogin, description, private)
	f.nextRepoID++
	key := viewerLogin + "/" + name
	f.Repos[key] = repo
	f.reposOrder = append(f.reposOrder, repo)
	return repo
}

func (f *Forge) findRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return f.Repos[owner+"/"+name]
}

func (f *Forge) deleteRepo(owner, name string) bool {
	key := owner + "/" + name
	repo, ok := f.Repos[key]
	if !ok {
		return false
	}
	delete(f.Repos, key)
	for i, r := range f.reposOrder {
		if r == repo {
			f.reposOrder = append(f.reposOrder[:i], f.reposOrder[i+1:]...)
			break
		}
	}
	return true
}

func (f *Forge) renameRepo(repo *Repo, newName string) {
	oldKey := repo.Owner + "/" + repo.Name
	delete(f.Repos, oldKey)
	for i, r := range f.reposOrder {
		if r == repo {
			f.reposOrder = append(f.reposOrder[:i], f.reposOrder[i+1:]...)
			break
		}
	}
	repo.Name = newName
	f.Repos[repo.Owner+"/"+newName] = repo
	f.reposOrder = append(f.reposOrder, repo)
}

// createLabel creates a label (or returns the existing one with that name).
func (f *Forge) createLabel(repo *Repo, name, color string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	label := &Label{Name: name, Color: color}
	repo.Labels[name] = label
	repo.labelsOrder = append(repo.labelsOrder, name)
	return label
}

// addComment adds a comment to an issue and invalidates its cached JSON.
func (f *Forge) addComment(repo *Repo, issue *Issue, body string) *IssueComment {
	comment := &IssueComment{ID: repo.NextCommentID, Body: body}
	repo.NextCommentID++
	issue.Comments = append(issue.Comments, *comment)
	delete(repo.IssueJSONCache, issue.Number)
	return comment
}

func (f *Forge) createIssue(repo *Repo, title, body string, labels []string) *Issue {
	number := repo.NextNumber
	repo.NextNumber++
	issue := &Issue{
		ID: f.nextIssueID, Number: number, Title: title, Body: body,
		State: "open", Author: viewerLogin, Labels: labels,
		CreatedAt: fixedISO, UpdatedAt: fixedISO,
	}
	f.nextIssueID++
	repo.Issues[number] = issue
	return issue
}

func (f *Forge) createPull(repo *Repo, title, body, headBranch, baseBranch string) *PullRequest {
	number := repo.NextNumber
	repo.NextNumber++
	headTip, baseTip := "", ""
	if oid, ok := repo.Refs["refs/heads/"+headBranch]; ok {
		headTip = oid
	}
	if oid, ok := repo.Refs["refs/heads/"+baseBranch]; ok {
		baseTip = oid
	}
	pull := &PullRequest{
		ID: f.nextPullID, Number: number, Title: title, Body: body,
		State: "open", Author: viewerLogin,
		HeadBranch: headBranch, BaseBranch: baseBranch,
		HeadSha: headTip, BaseSha: baseTip,
		CreatedAt: fixedISO, UpdatedAt: fixedISO,
	}
	f.nextPullID++
	repo.Pulls[number] = pull
	return pull
}

func (f *Forge) baseTip(repo *Repo, pull *PullRequest) string {
	if oid, ok := repo.Refs["refs/heads/"+pull.BaseBranch]; ok {
		return oid
	}
	return pull.BaseSha
}

func (f *Forge) pullFiles(repo *Repo, pull *PullRequest) []FileChange {
	base := mergeBase(repo.Objects, pull.HeadSha, f.baseTip(repo, pull))
	baseTree := ""
	if base != "" {
		if commit := repo.Objects[base]; commit != nil {
			baseTree = commit.parsedCommit().Tree
		}
	}
	headTree := ""
	if commit := repo.Objects[pull.HeadSha]; commit != nil {
		headTree = commit.parsedCommit().Tree
	}
	return diffTrees(repo.Objects, baseTree, headTree)
}

func (f *Forge) pullMergeable(repo *Repo, pull *PullRequest) bool {
	baseTip := f.baseTip(repo, pull)
	base := mergeBase(repo.Objects, pull.HeadSha, baseTip)
	if base == "" {
		return true
	}
	baseTree := ""
	if commit := repo.Objects[base]; commit != nil {
		baseTree = commit.parsedCommit().Tree
	}
	headTree := ""
	if commit := repo.Objects[pull.HeadSha]; commit != nil {
		headTree = commit.parsedCommit().Tree
	}
	tipTree := ""
	if commit := repo.Objects[baseTip]; commit != nil {
		tipTree = commit.parsedCommit().Tree
	}
	oldChanges := diffTrees(repo.Objects, baseTree, headTree)
	newChanges := diffTrees(repo.Objects, baseTree, tipTree)
	newByPath := map[string]FileChange{}
	for _, c := range newChanges {
		newByPath[c.Path] = c
	}
	for _, change := range oldChanges {
		if other, ok := newByPath[change.Path]; ok {
			if other.NewOID != change.NewOID {
				return false
			}
		}
	}
	return true
}

// mergePull squashes a pull request onto its base branch. Returns the merge commit sha,
// or "" when the pull cannot be merged (nil signals the caller to answer 405 with its reason).
func (f *Forge) mergePull(repo *Repo, pull *PullRequest) string {
	if pull.State != "open" || pull.Merged || !f.pullMergeable(repo, pull) {
		return ""
	}
	headCommit := repo.Objects[pull.HeadSha]
	headTree := ""
	if headCommit != nil {
		headTree = headCommit.parsedCommit().Tree
	}
	baseTip := f.baseTip(repo, pull)
	parents := []string{baseTip}
	message := fmt.Sprintf("Merge pull request #%d from %s/%s\n\n%s", pull.Number, viewerLogin, pull.HeadBranch, pull.Title)
	commitData := buildCommit(headTree, parents, message, "", "", fixedEpochSec)
	sha := hashObject(objCommit, commitData)
	repo.Objects[sha] = &GitObject{Type: objCommit, Data: commitData}
	repo.setRef("refs/heads/"+pull.BaseBranch, sha)
	pull.State = "closed"
	pull.Merged = true
	pull.MergeCommitSha = sha
	pull.MergedAt = fixedISO
	pull.ClosedAt = fixedISO
	return sha
}

// commitFile writes one file into a branch, creating the branch if needed.
// Returns the new commit sha and the blob sha.
func (f *Forge) commitFile(repo *Repo, branch, path, content string, message string) (string, string) {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	blobOID := hashObject(objBlob, []byte(content))
	repo.Objects[blobOID] = &GitObject{Type: objBlob, Data: []byte(content)}
	parents := []string{}
	parentTree := ""
	tip := ""
	if oid, ok := repo.Refs["refs/heads/"+branch]; ok {
		tip = oid
	}
	if tip != "" {
		if commit := repo.Objects[tip]; commit != nil {
			parents = append(parents, tip)
			parentTree = commit.parsedCommit().Tree
		}
	}
	rootOID := updateTree(repo.Objects, parentTree, path, blobOID)
	msg := message
	if msg == "" {
		msg = "Update " + path
	}
	commitData := buildCommit(rootOID, parents, msg, "", "", fixedEpochSec)
	sha := hashObject(objCommit, commitData)
	repo.Objects[sha] = &GitObject{Type: objCommit, Data: commitData}
	repo.setRef("refs/heads/"+branch, sha)
	return sha, blobOID
}

// deleteFile removes one file from a branch; false when the branch or file is absent.
func (f *Forge) deleteFile(repo *Repo, branch, path, message string) (string, bool) {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip := ""
	if oid, ok := repo.Refs["refs/heads/"+branch]; ok {
		tip = oid
	}
	if tip == "" {
		return "", false
	}
	commit := repo.Objects[tip]
	if commit == nil {
		return "", false
	}
	if readFileAt(repo.Objects, commit.parsedCommit().Tree, path) == nil {
		return "", false
	}
	rootOID := updateTree(repo.Objects, commit.parsedCommit().Tree, path, "")
	msg := message
	if msg == "" {
		msg = "Delete " + path
	}
	commitData := buildCommit(rootOID, []string{tip}, msg, "", "", fixedEpochSec)
	sha := hashObject(objCommit, commitData)
	repo.Objects[sha] = &GitObject{Type: objCommit, Data: commitData}
	repo.setRef("refs/heads/"+branch, sha)
	return sha, true
}
