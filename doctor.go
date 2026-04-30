package loto

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DriftClass identifies one of the five doctor drift classes from the north star.
type DriftClass string

const (
	DriftStaleTag      DriftClass = "stale_tag"
	DriftDeadPID       DriftClass = "dead_pid"
	DriftOrphaned      DriftClass = "orphaned"
	DriftLayoutDrift   DriftClass = "layout_drift"
	DriftSoftStaleHeld DriftClass = "soft_stale_held"
	DriftZombieHeld    DriftClass = "zombie_held"
)

// defaultZombieIdle is the inactivity threshold doctor applies when LOTO.ZombieIdle is zero.
const defaultZombieIdle = 30 * time.Minute

func (l *LOTO) zombieIdleThreshold() time.Duration {
	if l.ZombieIdle > 0 {
		return l.ZombieIdle
	}
	return defaultZombieIdle
}

// lastActivity returns the most recent observable touch for a held lock:
// tag acquisition time, mailbox file mtime, and target file mtime.
// msgsPath and target may be empty (global locks); empty inputs are skipped.
func lastActivity(tag *Tag, msgsPath, target string) time.Time {
	last := tag.Timestamp
	if msgsPath != "" {
		if fi, err := os.Stat(msgsPath); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	if target != "" {
		if fi, err := os.Stat(target); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last
}

// Finding is one item in a DoctorReport.
type Finding struct {
	Class       DriftClass `json:"class"`
	Path        string     `json:"path"`
	Target      string     `json:"target,omitempty"`
	AgentID     string     `json:"agent_id,omitempty"`
	Detail      string     `json:"detail"`
	Repaired    bool       `json:"repaired,omitempty"`
	WouldRepair bool       `json:"would_repair,omitempty"`
}

// DoctorReport is the result of a Doctor run.
type DoctorReport struct {
	Clean    bool      `json:"clean"`
	Findings []Finding `json:"findings,omitempty"`
}

// DoctorMode controls whether Doctor only reports or also repairs.
type DoctorMode int

const (
	DoctorCheck  DoctorMode = iota // report only (default)
	DoctorDryRun                   // show what --repair would do
	DoctorRepair                   // apply safe repairs
)

// Doctor walks the coordination base and detects the five drift classes defined
// in the north star. mode controls whether findings are reported or repaired.
// Returns DoctorReport.Clean=false and exit-hint 1 when drift is found.
func (l *LOTO) Doctor(byAgent string, mode DoctorMode) (*DoctorReport, error) {
	var findings []Finding

	gf, err := l.examineGlobalTag(byAgent, mode)
	if err != nil {
		return nil, err
	}
	findings = append(findings, gf...)

	ff, err := l.examineFileTags(byAgent, mode)
	if err != nil {
		return nil, err
	}
	findings = append(findings, ff...)

	lf, err := l.examineLayout()
	if err != nil {
		return nil, err
	}
	findings = append(findings, lf...)

	return &DoctorReport{Clean: len(findings) == 0, Findings: findings}, nil
}

// examineGlobalTag checks the global lock+tag pair.
func (l *LOTO) examineGlobalTag(byAgent string, mode DoctorMode) ([]Finding, error) {
	globalLockPath, globalTagPath := l.globalPaths()
	data, err := os.ReadFile(globalTagPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ErrSystem{Op: "doctor: read global tag", Err: err}
	}
	var tag Tag
	if json.Unmarshal(data, &tag) != nil {
		f := Finding{
			Class:  DriftOrphaned,
			Path:   globalTagPath,
			Detail: "global tag unreadable (corrupt JSON)",
		}
		switch mode {
		case DoctorRepair:
			if os.Remove(globalTagPath) == nil {
				f.Repaired = true
			}
		case DoctorDryRun:
			f.WouldRepair = true
		case DoctorCheck:
		}
		return []Finding{f}, nil
	}
	return l.examineTagPair(globalLockPath, globalTagPath, "global", &tag, byAgent, mode, true)
}

// examineFileTags walks <base>/files/ and checks each lock+tag pair plus orphan conditions.
//
//nolint:gocognit,gocyclo,cyclop,funlen // tracked: loto-dit (refactor pending)
func (l *LOTO) examineFileTags(byAgent string, mode DoctorMode) ([]Finding, error) {
	filesDir := filepath.Join(l.baseDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ErrSystem{Op: "doctor: read files dir", Err: err}
	}

	type pair struct{ hasLock, hasTag bool }
	pairs := map[string]*pair{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".lock" && ext != ".tag" && ext != ".msgs" {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ext)
		if pairs[base] == nil {
			pairs[base] = &pair{}
		}
		switch ext {
		case ".lock":
			pairs[base].hasLock = true
		case ".tag":
			pairs[base].hasTag = true
		}
	}

	var findings []Finding
	for base, p := range pairs {
		lockPath := filepath.Join(filesDir, base+".lock")
		tagPath := filepath.Join(filesDir, base+".tag")

		// .lock without .tag is the normal state after a clean release — skip.
		if !p.hasTag {
			continue
		}

		if p.hasTag && !p.hasLock {
			data, _ := os.ReadFile(tagPath)
			var tag Tag
			_ = json.Unmarshal(data, &tag)
			f := Finding{
				Class:   DriftOrphaned,
				Path:    tagPath,
				Target:  tag.Target,
				AgentID: tag.AgentID,
				Detail:  "tag file has no matching lock",
			}
			switch mode {
			case DoctorRepair:
				if os.Remove(tagPath) == nil {
					f.Repaired = true
				}
			case DoctorDryRun:
				f.WouldRepair = true
			case DoctorCheck:
			}
			findings = append(findings, f)
			continue
		}

		// Both exist — read the tag.
		data, err := os.ReadFile(tagPath)
		if err != nil {
			continue
		}
		var tag Tag
		if json.Unmarshal(data, &tag) != nil {
			findings = append(findings, Finding{
				Class:  DriftOrphaned,
				Path:   tagPath,
				Detail: "tag file unreadable (corrupt JSON)",
			})
			continue
		}

		pf, err := l.examineTagPair(lockPath, tagPath, tag.Target, &tag, byAgent, mode, false)
		if err != nil {
			return nil, err
		}
		findings = append(findings, pf...)

		// Only check target-exists if the pair itself looks healthy (lock held, PID alive).
		if len(pf) == 0 && tag.Target != "" {
			if _, statErr := os.Stat(tag.Target); os.IsNotExist(statErr) {
				f := Finding{
					Class:   DriftOrphaned,
					Path:    tagPath,
					Target:  tag.Target,
					AgentID: tag.AgentID,
					Detail:  fmt.Sprintf("target path %q no longer exists on disk", tag.Target),
				}
				switch mode {
				case DoctorRepair:
					_ = os.Remove(tagPath)
					_ = os.Remove(lockPath)
					f.Repaired = true
				case DoctorDryRun:
					f.WouldRepair = true
				case DoctorCheck:
				}
				findings = append(findings, f)
			}
		}
	}

	return findings, nil
}

// examineTagPair checks a lock+tag pair for drift classes 1, 2, and 5.
// isGlobal=true skips ForceBreak (which only handles file targets).
//
//nolint:gocognit,gocyclo,cyclop,funlen // tracked: loto-dit (refactor pending)
func (l *LOTO) examineTagPair(lockPath, tagPath, displayTarget string, tag *Tag, byAgent string, mode DoctorMode, isGlobal bool) ([]Finding, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &ErrSystem{Op: "doctor: open lock", Err: err}
	}
	defer f.Close()

	lockErr := flockExclusive(f)
	lockFree := lockErr == nil
	if lockFree {
		_ = flockRelease(f)
	}

	if lockFree {
		// Class 1: stale_tag — lock is free but a tag was left behind.
		fi := Finding{
			Class:   DriftStaleTag,
			Path:    tagPath,
			Target:  displayTarget,
			AgentID: tag.AgentID,
			Detail:  fmt.Sprintf("tag present but lock unheld (last holder: pid %d, agent %s)", tag.PID, tag.AgentID),
		}
		switch mode {
		case DoctorRepair:
			if os.Remove(tagPath) == nil {
				fi.Repaired = true
			}
		case DoctorDryRun:
			fi.WouldRepair = true
		case DoctorCheck:
		}
		return []Finding{fi}, nil
	}

	// Lock is held. Check PID.
	if !pidAlive(tag.PID) {
		// Class 2: dead_pid — flock held but the tagging process is gone.
		fi := Finding{
			Class:   DriftDeadPID,
			Path:    tagPath,
			Target:  displayTarget,
			AgentID: tag.AgentID,
			Detail:  fmt.Sprintf("lock held but tag PID %d is dead (agent %s)", tag.PID, tag.AgentID),
		}
		switch mode {
		case DoctorRepair:
			body := fmt.Sprintf("doctor: lock on %q force-broken by %s: recorded PID %d is dead", displayTarget, byAgent, tag.PID)
			_ = l.sendMsgBestEffort(displayTarget, byAgent, tag.AgentID, body, isGlobal)
			if isGlobal {
				if os.Remove(tagPath) == nil {
					fi.Repaired = true
				}
			} else if l.ForceBreak(displayTarget, byAgent, fmt.Sprintf("doctor: PID %d is dead", tag.PID)) == nil {
				fi.Repaired = true
			}
		case DoctorDryRun:
			fi.WouldRepair = true
		case DoctorCheck:
		}
		return []Finding{fi}, nil
	}

	// Lock held, PID alive. Check activity-based staleness (zombie):
	// agent process exists but hasn't refreshed tag, sent mail, or touched
	// target within the idle threshold.
	var msgsPath string
	if !isGlobal {
		if mp, perr := l.msgsPath(displayTarget); perr == nil {
			msgsPath = mp
		}
	}
	target := ""
	if !isGlobal {
		target = displayTarget
	}
	la := lastActivity(tag, msgsPath, target)
	if !la.IsZero() && time.Since(la) > l.zombieIdleThreshold() {
		fi := Finding{
			Class:   DriftZombieHeld,
			Path:    tagPath,
			Target:  displayTarget,
			AgentID: tag.AgentID,
			Detail:  fmt.Sprintf("lock held by pid %d (agent %s) but no activity since %s (idle %s > threshold %s)", tag.PID, tag.AgentID, la.Format(time.RFC3339), time.Since(la).Round(time.Second), l.zombieIdleThreshold()),
		}
		switch mode {
		case DoctorRepair:
			body := fmt.Sprintf("doctor: lock on %q force-broken by %s: zombie (no activity since %s)", displayTarget, byAgent, la.Format(time.RFC3339))
			_ = l.sendMsgBestEffort(displayTarget, byAgent, tag.AgentID, body, isGlobal)
			if isGlobal {
				if os.Remove(tagPath) == nil {
					fi.Repaired = true
				}
			} else if l.ForceBreak(displayTarget, byAgent, fmt.Sprintf("doctor: zombie idle %s", time.Since(la).Round(time.Second))) == nil {
				fi.Repaired = true
			}
		case DoctorDryRun:
			fi.WouldRepair = true
		case DoctorCheck:
		}
		return []Finding{fi}, nil
	}

	// Soft-TTL expiry: report-only.
	if tag.SoftStale() {
		return []Finding{{
			Class:   DriftSoftStaleHeld,
			Path:    tagPath,
			Target:  displayTarget,
			AgentID: tag.AgentID,
			Detail:  fmt.Sprintf("soft TTL expired at %s but lock still held by pid %d (agent %s) — report only", tag.ExpiresAt.Format(time.RFC3339), tag.PID, tag.AgentID),
		}}, nil
	}

	return nil, nil
}

// examineLayout checks for unexpected entries directly in the coordination base.
func (l *LOTO) examineLayout() ([]Finding, error) {
	entries, err := os.ReadDir(l.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ErrSystem{Op: "doctor: read base dir", Err: err}
	}

	whitelist := map[string]bool{
		"global.lock":  true,
		"global.tag":   true,
		"files":        true,
		"reservations": true,
	}

	var findings []Finding
	for _, e := range entries {
		if !whitelist[e.Name()] {
			findings = append(findings, Finding{
				Class:  DriftLayoutDrift,
				Path:   filepath.Join(l.baseDir, e.Name()),
				Detail: fmt.Sprintf("unexpected entry %q in coordination base (report only)", e.Name()),
			})
		}
	}
	return findings, nil
}

// sendMsgBestEffort sends a mailbox message, ignoring errors. For global locks
// the target string is not a real path, so we skip the mailbox silently.
func (l *LOTO) sendMsgBestEffort(target, from, to, body string, isGlobal bool) error {
	if isGlobal {
		return nil
	}
	return l.SendMsg(target, from, to, body, true)
}
