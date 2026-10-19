// Package tasks provides module-level functionality for tasks.
// input: the task's uploaded catalog rows and the configured object lister
// output: objectView, the chain.objects facts (listing completeness per prefix, unjudged keys uploaded after the snapshot, NoObjectStorage); object kinds that an incomplete listing must not judge, including I6_ORPHAN_ROW (design 0009 §21, §26.4)
// pos: P2-5.2 object listing for the health evaluator
// note: if this file changes, update this header and module README.md.
package tasks

// evaluator_objects.go: the object facts of design 0009 §21 (P2-5.2). One
// listing per object prefix, cached for health.object_list_interval_sec;
// a truncated or failed listing leaves chain.objects incomplete and judges
// nothing. A key uploaded after its prefix was listed is not judged by
// that listing (§21.3); it is relisted no sooner than objectRelistMin.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/health"
)

const (
	defaultObjectListInterval = 300 * time.Second
	defaultObjectListLimit    = 10000
	objectRelistMin           = 30 * time.Second
	objectListTimeout         = 30 * time.Second
)

// objectKinds are the assurance kinds judged from an object listing.
var objectKinds = map[assurance.Kind]bool{assurance.I6MissingObject: true, assurance.I6ObjectSizeMismatch: true, assurance.I6OrphanRow: true}

type objectListing struct {
	at    time.Time
	objs  map[string]int64
	ok    bool
	known map[string]bool // keys UPLOADED in the catalog when listed
	gen   int64
}

// objectView is what the chain is given about object storage for one pass.
type objectView struct {
	storage  bool
	objects  map[string]int64
	prefixes map[string]bool
	grace    time.Duration
	unjudged map[string]bool
	key      string
}

func perTask(global time.Duration, task map[string]time.Duration, id string) time.Duration {
	if d, ok := task[id]; ok && d > 0 {
		return d
	}
	return global
}

func anyIn(segs []string, set map[string]bool) bool {
	for _, s := range segs {
		if set[s] {
			return true
		}
	}
	return false
}

func (e *Evaluator) objectView(taskID string, now time.Time, rows []assurance.Row, facts *health.Facts) objectView {
	e.s.mu.Lock()
	up := e.s.fileUploader
	e.s.mu.Unlock()
	v := objectView{grace: perTask(e.cfg.UploadGrace, e.cfg.UploadGraceTask, taskID)}
	if facts != nil {
		facts.UploadLateCriticalAfter = perTask(e.cfg.UploadLateCriticalAfter, e.cfg.UploadLateCriticalAfterTask, taskID)
	}
	if up == nil {
		// §21.2: no uploader is the confirmed fact "no object storage".
		if facts != nil {
			e.mu.Lock()
			was := e.objectConfig
			e.mu.Unlock()
			if was == "" {
				was = "unknown"
			}
			facts.NoObjectStorage = true
			facts.ObjectConfigAudit = "was " + was
		}
		v.key = fmt.Sprintf("none|%d", v.grace)
		return v
	}
	v.storage = true
	e.mu.Lock()
	e.objectConfig = fmt.Sprintf("%T", up)
	e.mu.Unlock()
	if facts != nil {
		facts.Produced = map[health.Scope]bool{health.ScopeChainObjects: true}
		for s, ok := range health.Produced {
			facts.Produced[s] = ok
		}
	}
	byPrefix := map[string][]assurance.Row{}
	for _, r := range rows {
		key := strings.TrimSpace(r.ObjectKey)
		if r.UploadState != "UPLOADED" || key == "" {
			continue
		}
		p := assurance.ObjectPrefixOf(key)
		byPrefix[p] = append(byPrefix[p], r)
	}
	lister, canList := up.(ObjectLister)
	v.objects, v.prefixes, v.unjudged = map[string]int64{}, map[string]bool{}, map[string]bool{}
	segs := map[string]bool{}
	complete := canList
	prefixes := make([]string, 0, len(byPrefix))
	for p := range byPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	var keyParts []string
	for _, p := range prefixes {
		if !canList {
			break
		}
		l := e.listing(lister, taskID, p, now, byPrefix[p])
		if l == nil || !l.ok {
			complete = false
			continue
		}
		keyParts = append(keyParts, fmt.Sprintf("%s=%d", p, l.gen))
		v.prefixes[p] = true
		for k, n := range l.objs {
			v.objects[k] = n
		}
		for _, r := range byPrefix[p] {
			if l.known[strings.TrimSpace(r.ObjectKey)] {
				segs[r.Name] = true
			} else {
				v.unjudged[r.Name] = true
			}
		}
	}
	if !canList {
		v.objects, v.prefixes = nil, nil
	}
	if facts != nil {
		facts.Complete[health.ScopeChainObjects] = complete
		facts.ObjectSegments = segs
	}
	un := make([]string, 0, len(v.unjudged))
	for s := range v.unjudged {
		un = append(un, s)
	}
	sort.Strings(un)
	v.key = fmt.Sprintf("s|%d|%t|%s|%s", v.grace, canList, strings.Join(keyParts, ","), strings.Join(un, ","))
	return v
}

// listing returns the cached listing of prefix for the task, relisting it
// when the interval passed, or when a key uploaded since is neither known
// nor listed and objectRelistMin passed.
func (e *Evaluator) listing(lister ObjectLister, taskID, prefix string, now time.Time, rows []assurance.Row) *objectListing {
	ck := taskID + "\x00" + prefix
	e.mu.Lock()
	l := e.listings[ck]
	e.mu.Unlock()
	stale := l == nil || now.Sub(l.at) >= e.cfg.ObjectListInterval || now.Before(l.at)
	if !stale && now.Sub(l.at) >= objectRelistMin {
		for _, r := range rows {
			k := strings.TrimSpace(r.ObjectKey)
			if _, listed := l.objs[k]; !l.known[k] && !listed {
				stale = true
				break
			}
		}
	}
	if !stale {
		return l
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[strings.TrimSpace(r.ObjectKey)] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), objectListTimeout)
	objs, truncated, err := lister.ListObjects(ctx, prefix, e.cfg.ObjectListLimit)
	cancel()
	e.mu.Lock()
	e.listingGen++
	nl := &objectListing{at: now, objs: objs, ok: err == nil && !truncated, known: known, gen: e.listingGen}
	e.listings[ck] = nl
	e.mu.Unlock()
	return nl
}
