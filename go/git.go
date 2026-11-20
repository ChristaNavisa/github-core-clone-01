// The git smart-HTTP surface: v0/v2 advertisements, upload-pack (fetch +
// ls-refs) and receive-pack, plus the fetch planner. This is where the push
// and clone workloads live, so the hot paths are built around caches:
// advertisements are cached on the exact ref state and pack bytes on the
// exact sorted object list, because objects are immutable and a fetch plan
// for one object set is valid forever.

package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

const uploadCapsV0 = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack"
const receiveCapsV0 = "symref=HEAD:refs/heads/main report-status side-band-64k delete-refs ofs-delta"

func tagPeel(repo *Repo, oid string) string {
	obj := repo.Objects[oid]
	if obj == nil || obj.Type != objTag {
		return ""
	}
	return parseTag(obj.Data).Object
}

// advertisementV0 is a v0 ref advertisement, cached on the exact ref state.
func advertisementV0(repo *Repo, service string) []byte {
	caps := uploadCapsV0
	if service == "git-receive-pack" {
		caps = receiveCapsV0
	}
	var sb strings.Builder
	for i, name := range repo.refsOrder {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(name)
		sb.WriteByte('=')
		sb.WriteString(repo.Refs[name])
	}
	key := service + "|" + sb.String()
	if cached, ok := repo.AdV0Cache[key]; ok {
		return cached
	}
	var out bytes.Buffer
	out.Write(pktLine([]byte(fmt.Sprintf("# service=%s\n", service))))
	out.Write(pktFlush())
	names := sortedRefNames(repo)
	if len(names) == 0 {
		out.Write(pktLine([]byte(fmt.Sprintf("%s capabilities^{}\x00%s\n", zeroOID, caps))))
	} else {
		out.Write(pktLine([]byte(fmt.Sprintf("%s HEAD\x00%s\n", repo.headTip(), caps))))
		for _, name := range names {
			oid := repo.Refs[name]
			out.Write(pktLine([]byte(fmt.Sprintf("%s %s\n", oid, name))))
			if peeled := tagPeel(repo, oid); peeled != "" {
				out.Write(pktLine([]byte(fmt.Sprintf("%s %s^{}\n", peeled, name))))
			}
		}
	}
	out.Write(pktFlush())
	body := out.Bytes()
	if len(repo.AdV0Cache) >= 8 {
		repo.AdV0Cache = map[string][]byte{}
	}
	repo.AdV0Cache[key] = body
	return body
}

var advertisementV2UploadPack = func() []byte {
	var out bytes.Buffer
	out.Write(pktLine([]byte("# service=git-upload-pack\n")))
	out.Write(pktFlush())
	out.Write(pktLine([]byte("version 2\n")))
	out.Write(pktLine([]byte("ls-refs=unborn\n")))
	out.Write(pktLine([]byte("fetch=shallow\n")))
	out.Write(pktLine([]byte("object-format=sha1\n")))
	out.Write(pktFlush())
	return out.Bytes()
}()

var emptyPack = writePack(nil)

// cachedPack returns pack bytes for a fetch plan, cached on the exact sorted
// object list. Objects are immutable, so a pack built for one object set is
// valid forever; the clone workload fetches the same set dozens of times.
func cachedPack(repo *Repo, sortedOIDs []string) []byte {
	key := strings.Join(sortedOIDs, ",") + ","
	if hit, ok := repo.PackCache[key]; ok {
		return hit
	}
	objects := make([]*GitObject, 0, len(sortedOIDs))
	for _, oid := range sortedOIDs {
		if obj := repo.Objects[oid]; obj != nil {
			objects = append(objects, obj)
		}
	}
	pack := writePack(objects)
	if len(repo.PackCache) >= 32 {
		repo.PackCache = map[string][]byte{}
	}
	repo.PackCache[key] = pack
	return pack
}

// ---------------------------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------------------------

// heldClosure is the set of objects a client holding `haves` (with `shallow`
// boundaries) already has.
func heldClosure(repo *Repo, haves []string, shallowSet map[string]bool) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), haves...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case objCommit:
			c := obj.parsedCommit()
			stack = append(stack, c.Tree)
			if !shallowSet[oid] {
				stack = append(stack, c.Parents...)
			}
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
	return seen
}

func addTreeClosure(repo *Repo, rootOID string, out map[string]bool) {
	stack := []string{rootOID}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = true
		if obj.Type == objTree {
			for _, e := range obj.parsedTree() {
				stack = append(stack, e.OID)
			}
		}
	}
}

// effectiveDepth is the depth limit for one want. `deepen N` on its own is
// an absolute total depth; with deepen-relative it is N more generations
// below the client's current shallow boundary (the distance from the want to
// the nearest boundary it sent, plus one for the boundary itself, plus N).
func effectiveDepth(repo *Repo, want string, shallowBoundaries []string, depth int, relative bool) int {
	if !relative {
		return depth
	}
	targets := map[string]bool{}
	for _, s := range shallowBoundaries {
		targets[s] = true
	}
	if len(targets) == 0 {
		return depth
	}
	distance := 0
	frontier := []string{want}
	seen := map[string]bool{want: true}
	for len(frontier) > 0 {
		for _, oid := range frontier {
			if targets[oid] {
				return distance + 1 + depth
			}
		}
		var next []string
		for _, oid := range frontier {
			obj := repo.Objects[oid]
			if obj == nil || obj.Type != objCommit {
				continue
			}
			for _, p := range obj.parsedCommit().Parents {
				if !seen[p] {
					seen[p] = true
					next = append(next, p)
				}
			}
		}
		frontier = next
		distance++
	}
	return depth
}

type fetchPlan struct {
	sortedOIDs []string // included objects, sorted, minus held
	shallow    []string
	unshallow  []string
}

type queueEntry struct {
	oid   string
	dist  int
	limit *int // nil = unlimited
}

func planFetch(repo *Repo, wants, haves, shallowBoundaries []string, depth *int, relative bool) fetchPlan {
	shallowSet := map[string]bool{}
	for _, s := range shallowBoundaries {
		shallowSet[s] = true
	}
	held := heldClosure(repo, haves, shallowSet)

	included := map[string]bool{}
	var boundary []string
	var queue []queueEntry
	for _, w := range wants {
		if repo.Objects[w] == nil {
			continue
		}
		var limit *int
		if depth != nil {
			d := effectiveDepth(repo, w, shallowBoundaries, *depth, relative)
			limit = &d
		}
		queue = append(queue, queueEntry{oid: w, dist: 0, limit: limit})
	}
	// The largest limit under which each commit was already expanded: a commit
	// reached again under a larger limit is expanded again.
	seenLimits := map[string]*int{}
	head := 0
	for head < len(queue) {
		entry := queue[head]
		head++
		seen, ok := seenLimits[entry.oid]
		// Skip only when this commit was already expanded with an equal or
		// larger limit (nil means unlimited, which covers everything).
		if ok && (seen == nil || (entry.limit != nil && *entry.limit <= *seen)) {
			continue
		}
		seenLimits[entry.oid] = entry.limit
		obj := repo.Objects[entry.oid]
		if obj == nil {
			continue
		}
		if obj.Type == objTag {
			included[entry.oid] = true
			target := parseTag(obj.Data).Object
			queue = append(queue, queueEntry{oid: target, dist: entry.dist, limit: entry.limit})
			continue
		}
		if obj.Type != objCommit {
			included[entry.oid] = true
			continue
		}
		included[entry.oid] = true
		c := obj.parsedCommit()
		addTreeClosure(repo, c.Tree, included)
		if entry.limit == nil || entry.dist+1 < *entry.limit {
			for _, p := range c.Parents {
				queue = append(queue, queueEntry{oid: p, dist: entry.dist + 1, limit: entry.limit})
			}
		} else if len(c.Parents) > 0 {
			boundary = append(boundary, entry.oid)
		}
	}

	var sorted []string
	for oid := range included {
		sorted = append(sorted, oid)
	}
	sort.Strings(sorted)
	var objects []string
	for _, oid := range sorted {
		if held[oid] {
			continue
		}
		if repo.Objects[oid] != nil {
			objects = append(objects, oid)
		}
	}

	var unshallow []string
	for _, s := range shallowBoundaries {
		if !included[s] && !held[s] {
			continue
		}
		obj := repo.Objects[s]
		if obj == nil || obj.Type != objCommit {
			continue
		}
		parents := obj.parsedCommit().Parents
		all := true
		for _, p := range parents {
			if !included[p] && !held[p] {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, s)
		}
	}
	shallow := []string{}
	if depth != nil {
		shallow = boundary
	}
	return fetchPlan{sortedOIDs: objects, shallow: shallow, unshallow: unshallow}
}

func fetchResponseV2(plan fetchPlan, haves []string, repo *Repo) []byte {
	var out bytes.Buffer
	// The acknowledgments section exists only when the client sent haves.
	if len(haves) > 0 {
		out.Write(pktLine([]byte("acknowledgments\n")))
		acked := false
		for _, have := range haves {
			if repo.Objects[have] != nil {
				out.Write(pktLine([]byte(fmt.Sprintf("ACK %s\n", have))))
				acked = true
			}
		}
		if !acked {
			out.Write(pktLine([]byte("NAK\n")))
		}
		out.Write(pktLine([]byte("ready\n")))
		out.Write(pktDelim())
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out.Write(pktLine([]byte("shallow-info\n")))
		for _, oid := range plan.shallow {
			out.Write(pktLine([]byte(fmt.Sprintf("shallow %s\n", oid))))
		}
		for _, oid := range plan.unshallow {
			out.Write(pktLine([]byte(fmt.Sprintf("unshallow %s\n", oid))))
		}
		out.Write(pktDelim())
	}
	out.Write(pktLine([]byte("packfile\n")))
	pack := cachedPack(repo, plan.sortedOIDs)
	out.Write(sidebandChunks(pack))
	out.Write(pktFlush())
	return out.Bytes()
}

func fetchResponseV0(plan fetchPlan, haves []string, repo *Repo) []byte {
	var out bytes.Buffer
	acked := false
	for _, have := range haves {
		if repo.Objects[have] != nil {
			out.Write(pktLine([]byte(fmt.Sprintf("ACK %s\n", have))))
			acked = true
		}
	}
	if !acked {
		out.Write(pktLine([]byte("NAK\n")))
	}
	for _, oid := range plan.shallow {
		out.Write(pktLine([]byte(fmt.Sprintf("shallow %s\n", oid))))
	}
	for _, oid := range plan.unshallow {
		out.Write(pktLine([]byte(fmt.Sprintf("unshallow %s\n", oid))))
	}
	pack := cachedPack(repo, plan.sortedOIDs)
	out.Write(sidebandChunks(pack))
	out.Write(pktFlush())
	return out.Bytes()
}

// ---------------------------------------------------------------------------------------------
// upload-pack / ls-refs / receive-pack / routing
// ---------------------------------------------------------------------------------------------

// gitRequest is everything a git handler needs from the HTTP layer.
type gitRequest struct {
	method  string
	segments []string
	query   map[string]string
	headers map[string]string
	body    []byte
}

func gitResult(status int, contentType string, body []byte) *HandlerResult {
	return &HandlerResult{
		Status:  status,
		Headers: map[string]string{"content-type": contentType},
		Body:    body,
	}
}

func handleUploadPack(repo *Repo, ctx gitRequest) *HandlerResult {
	isV2 := ctx.headers["git-protocol"] == "version=2"
	packets, _, err := decodePktLines(ctx.body, false)
	if err != nil {
		return errorResult(400, "malformed pkt-line stream")
	}
	lines := pktLinesText(packets)
	if isV2 {
		first := ""
		if len(lines) > 0 {
			first = stripLine(lines[0])
		}
		if first != "command=fetch" {
			return errorResult(400, "expected command=fetch")
		}
		var wants, haves, shallow []string
		var depth *int
		relative := false
		for _, line := range lines[1:] {
			text := stripLine(line)
			if text == "" || text == "thin-pack" || text == "no-progress" || text == "ofs-delta" || text == "include-tag" {
				continue
			}
			if strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
				continue
			}
			switch {
			case strings.HasPrefix(text, "want "):
				wants = append(wants, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "have "):
				haves = append(haves, strings.TrimSpace(text[5:]))
			case strings.HasPrefix(text, "shallow "):
				shallow = append(shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				var d int
				fmt.Sscanf(strings.TrimSpace(text[7:]), "%d", &d)
				depth = &d
			case text == "deepen-relative":
				relative = true
			case text == "done":
				continue
			case strings.HasPrefix(text, "filter "):
				return errorResult(400, "fetch filter is not supported")
			}
		}
		if len(wants) == 0 {
			var out bytes.Buffer
			if len(haves) > 0 {
				out.Write(pktLine([]byte("acknowledgments\n")))
				out.Write(pktLine([]byte("NAK\n")))
				out.Write(pktLine([]byte("ready\n")))
				out.Write(pktDelim())
			}
			out.Write(pktLine([]byte("packfile\n")))
			out.Write(sidebandChunks(emptyPack))
			out.Write(pktFlush())
			return gitResult(200, "application/x-git-upload-pack-result", out.Bytes())
		}
		var missing []string
		for _, w := range wants {
			if repo.Objects[w] == nil {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			var out bytes.Buffer
			if len(haves) > 0 {
				out.Write(pktLine([]byte("acknowledgments\n")))
				out.Write(pktLine([]byte("NAK\n")))
				out.Write(pktLine([]byte("ready\n")))
				out.Write(pktDelim())
			}
			out.Write(pktLine([]byte("packfile\n")))
			payload := append([]byte{3}, []byte(fmt.Sprintf("object %s not found\n", missing[0]))...)
			out.Write(pktLine(payload))
			out.Write(pktFlush())
			return gitResult(200, "application/x-git-upload-pack-result", out.Bytes())
		}
		plan := planFetch(repo, wants, haves, shallow, depth, relative)
		return gitResult(200, "application/x-git-upload-pack-result", fetchResponseV2(plan, haves, repo))
	}
	// v0: wants section, flush, haves section, flush.
	var wants, haves, shallow []string
	var depth *int
	section := 0
	for _, packet := range packets {
		if packet.Flush {
			section++
			continue
		}
		if packet.Data == nil {
			continue
		}
		text := stripLine(string(packet.Data))
		if section == 0 {
			switch {
			case strings.HasPrefix(text, "want "):
				rest := text[5:]
				if sp := strings.Index(rest, " "); sp >= 0 {
					rest = rest[:sp]
				}
				wants = append(wants, rest)
			case strings.HasPrefix(text, "shallow "):
				shallow = append(shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				var d int
				fmt.Sscanf(strings.TrimSpace(text[7:]), "%d", &d)
				depth = &d
			}
		} else if strings.HasPrefix(text, "have ") {
			haves = append(haves, strings.TrimSpace(text[5:]))
		}
	}
	if len(wants) == 0 {
		return gitResult(200, "application/x-git-upload-pack-result", fetchResponseV0(fetchPlan{}, haves, repo))
	}
	plan := planFetch(repo, wants, haves, shallow, depth, false)
	return gitResult(200, "application/x-git-upload-pack-result", fetchResponseV0(plan, haves, repo))
}

func handleLsRefs(repo *Repo, ctx gitRequest) *HandlerResult {
	packets, _, err := decodePktLines(ctx.body, false)
	if err != nil {
		return errorResult(400, "malformed pkt-line stream")
	}
	lines := pktLinesText(packets)
	first := ""
	if len(lines) > 0 {
		first = stripLine(lines[0])
	}
	if first != "command=ls-refs" {
		return errorResult(400, "expected command=ls-refs")
	}
	peel := false
	symrefs := false
	var prefixes []string
	for _, line := range lines[1:] {
		text := stripLine(line)
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			continue
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, text[len("ref-prefix "):])
		case strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format="):
			continue
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out bytes.Buffer
	names := sortedRefNames(repo)
	if symrefs && keep("HEAD") {
		tip := repo.headTip()
		if tip == zeroOID {
			out.Write(pktLine([]byte(fmt.Sprintf("unborn HEAD symref-target:refs/heads/%s\n", repo.DefaultBranch))))
		} else {
			out.Write(pktLine([]byte(fmt.Sprintf("%s HEAD symref-target:refs/heads/%s\n", tip, repo.DefaultBranch))))
		}
	}
	for _, name := range names {
		if !keep(name) {
			continue
		}
		oid := repo.Refs[name]
		line := fmt.Sprintf("%s %s", oid, name)
		if peel {
			if peeled := tagPeel(repo, oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		out.Write(pktLine([]byte(line + "\n")))
	}
	out.Write(pktFlush())
	return gitResult(200, "application/x-git-upload-pack-result", out.Bytes())
}

// closureComplete reports whether every object reachable from tip is
// present. Incremental: the walk prunes at objects already proven complete
// (repo.Verified), so a push that extends a verified tip only inspects the
// newly arrived objects. Objects are append-only, so a verified closure
// stays complete forever; on success every visited object becomes verified.
func closureComplete(repo *Repo, tip string) bool {
	stack := []string{tip}
	visited := map[string]bool{}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[oid] || repo.Verified[oid] {
			continue
		}
		obj := repo.Objects[oid]
		if obj == nil {
			return false
		}
		visited[oid] = true
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
	for oid := range visited {
		repo.Verified[oid] = true
	}
	return true
}

type receiveCommand struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(repo *Repo, ctx gitRequest) *HandlerResult {
	packets, _, err := decodePktLines(ctx.body, true)
	if err != nil {
		return errorResult(400, "malformed pkt-line stream")
	}
	flushPacket := (*pktLineItem)(nil)
	if len(packets) > 0 {
		flushPacket = &packets[len(packets)-1]
	}
	if flushPacket == nil || !flushPacket.Flush {
		return errorResult(400, "receive-pack command list has no flush")
	}
	packBytes := ctx.body[flushPacket.EndOff:]
	var commands []receiveCommand
	for _, packet := range packets {
		if packet.Data == nil {
			continue
		}
		text := string(packet.Data)
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		line := stripLine(text)
		parts := strings.Split(line, " ")
		if len(parts) < 3 || !isOID(parts[0]) || !isOID(parts[1]) || !strings.HasPrefix(parts[2], "refs/") {
			return errorResult(400, fmt.Sprintf("bad receive-pack command %s", jsonQuote(line[:min(60, len(line))])))
		}
		commands = append(commands, receiveCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	if len(commands) == 0 {
		return errorResult(400, "receive-pack sent no commands")
	}

	var reportLines []string
	unpackOk := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes)
		if err != nil {
			unpackOk = false
			unpackDetail = err.Error()
		} else {
			for oid, obj := range objects {
				if repo.Objects[oid] == nil {
					repo.Objects[oid] = obj
				}
			}
		}
	}
	if unpackOk {
		for _, cmd := range commands {
			current, exists := repo.Refs[cmd.ref]
			if cmd.newOID == zeroOID {
				if !exists {
					reportLines = append(reportLines, fmt.Sprintf("ng %s remote ref does not exist", cmd.ref))
				} else if cmd.oldOID != zeroOID && cmd.oldOID != current {
					reportLines = append(reportLines, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
				} else {
					repo.deleteRef(cmd.ref)
					reportLines = append(reportLines, fmt.Sprintf("ok %s", cmd.ref))
				}
			} else if cmd.oldOID == zeroOID {
				if exists {
					reportLines = append(reportLines, fmt.Sprintf("ng %s already exists", cmd.ref))
				} else if repo.Objects[cmd.newOID] == nil {
					reportLines = append(reportLines, fmt.Sprintf("ng %s missing object", cmd.ref))
				} else if !closureComplete(repo, cmd.newOID) {
					reportLines = append(reportLines, fmt.Sprintf("ng %s missing objects", cmd.ref))
				} else {
					repo.setRef(cmd.ref, cmd.newOID)
					reportLines = append(reportLines, fmt.Sprintf("ok %s", cmd.ref))
				}
			} else if !exists {
				reportLines = append(reportLines, fmt.Sprintf("ng %s remote ref does not exist", cmd.ref))
			} else if cmd.oldOID != current {
				reportLines = append(reportLines, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
			} else if cmd.newOID == current {
				reportLines = append(reportLines, fmt.Sprintf("ok %s", cmd.ref))
			} else if repo.Objects[cmd.newOID] == nil {
				reportLines = append(reportLines, fmt.Sprintf("ng %s missing object", cmd.ref))
			} else if !closureComplete(repo, cmd.newOID) {
				reportLines = append(reportLines, fmt.Sprintf("ng %s missing objects", cmd.ref))
			} else if !isAncestor(repo.Objects, current, cmd.newOID) {
				reportLines = append(reportLines, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
			} else {
				repo.setRef(cmd.ref, cmd.newOID)
				reportLines = append(reportLines, fmt.Sprintf("ok %s", cmd.ref))
			}
		}
	}
	// The report-status stream is itself pkt-lined inside band 1 and ends
	// with a flush inside the sideband data; the outer flush then ends the
	// response.
	reportText := "unpack " + unpackDetail + "\n"
	for _, l := range reportLines {
		reportText += l + "\n"
	}
	var reportStream bytes.Buffer
	for _, l := range strings.Split(reportText, "\n") {
		if l == "" {
			continue
		}
		reportStream.Write(pktLine([]byte(l + "\n")))
	}
	reportStream.Write(pktFlush())
	var body bytes.Buffer
	body.Write(sidebandChunks(reportStream.Bytes()))
	body.Write(pktFlush())
	return gitResult(200, "application/x-git-receive-pack-result", body.Bytes())
}

func handleGit(f *Forge, ctx gitRequest) *HandlerResult {
	if len(ctx.segments) < 2 {
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	owner := ctx.segments[0]
	repoGit := ctx.segments[1]
	if !strings.HasSuffix(repoGit, ".git") {
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	repoName := repoGit[:len(repoGit)-4]
	rest := ctx.segments[2:]
	service := ctx.query["service"]
	repo := f.findRepo(owner, repoName)
	if ctx.method == "GET" && len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" {
		if repo == nil {
			return jsonResult(404, map[string]any{"message": "Not Found"})
		}
		if service == "git-upload-pack" {
			isV2 := ctx.headers["git-protocol"] == "version=2"
			if isV2 {
				return gitResult(200, "application/x-git-upload-pack-advertisement", advertisementV2UploadPack)
			}
			return gitResult(200, "application/x-git-upload-pack-advertisement", advertisementV0(repo, "git-upload-pack"))
		}
		if service == "git-receive-pack" {
			return gitResult(200, "application/x-git-receive-pack-advertisement", advertisementV0(repo, "git-receive-pack"))
		}
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	if repo == nil {
		return jsonResult(404, map[string]any{"message": "Not Found"})
	}
	if ctx.method == "POST" && len(rest) == 1 && rest[0] == "git-upload-pack" {
		packets, _, err := decodePktLines(ctx.body, false)
		if err != nil {
			return errorResult(400, "malformed pkt-line stream")
		}
		lines := pktLinesText(packets)
		first := ""
		if len(lines) > 0 {
			first = stripLine(lines[0])
		}
		if first == "command=ls-refs" {
			return handleLsRefs(repo, ctx)
		}
		return handleUploadPack(repo, ctx)
	}
	if ctx.method == "POST" && len(rest) == 1 && rest[0] == "git-receive-pack" {
		return handleReceivePack(repo, ctx)
	}
	return jsonResult(404, map[string]any{"message": "Not Found"})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// jsonQuote renders a Go string as a JSON string literal (for the one error
// message that embeds JSON.stringify output).
func jsonQuote(s string) string {
	b, _ := jsonMarshalString(s)
	return string(b)
}
