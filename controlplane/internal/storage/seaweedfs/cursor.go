package seaweedfs

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// systemLogDir is the filer path under which metadata log files live
// (SeaweedFS 4.48, weed/filer/topics.go: SystemLogDir = "/topics/.system/log").
const systemLogDir = "/topics/.system/log"

// dayDirFormat and minuteFileFormat match the persisted metadata log layout
// written by SeaweedFS 4.48 (weed/filer/filer_notify.go, logFlushFunc):
//
//	fmt.Sprintf("%s/%04d-%02d-%02d/%02d-%02d.%08x", SystemLogDir,
//	    startTime.Year(), startTime.Month(), startTime.Day(),
//	    startTime.Hour(), startTime.Minute(), f.UniqueFilerId)
//
// with startTime in UTC: one day directory per day, one file per minute
// (the flush interval), named by the window's start minute plus the filer id
// in lower-case hex. Day directories and minute files are zero-padded, so
// lexicographic order equals chronological order — the reading side relies on
// that (weed/filer/filer_notify_read.go). The filer-id suffix is tolerated
// but not interpreted (it is a random per-process int32, not the persisted
// Filer.Signature).
const (
	dayDirFormat     = "2006-01-02"
	minuteFileFormat = "15-04"
)

// minuteFileRe matches "<HH-MM>[.<filer-id hex>]"; the optional id suffix is
// 1–16 hex digits.
var minuteFileRe = regexp.MustCompile(`^(\d{2}-\d{2})(?:\.([0-9a-fA-F]{1,16}))?$`)

// ErrEmptyLogName is returned by CursorBeforeEarliestLog for an empty name.
var ErrEmptyLogName = errors.New("seaweedfs: earliest log name is empty")

// ResumeFrom returns the ts_ns a re-subscription should start from: the last
// persisted declared value rewound by margin, floored at zero.
//
// The rewind covers filer clock rollback across restarts (the declared
// value's monotonic lower bound lives only in filer process memory, D-037);
// replayed duplicates are absorbed by the consumers' read-before-write
// behavior. A negative margin is clamped to zero: resuming after the declared
// value would silently skip events.
func ResumeFrom(declared int64, margin time.Duration) int64 {
	m := margin.Nanoseconds()
	if m < 0 {
		m = 0
	}
	resume := declared - m
	if resume < 0 {
		return 0
	}
	return resume
}

// GapWindow returns the re-check window [from, to] for object mtimes after a
// stream break: [min(C,H) − margin, max(C,H)], where C is the last declared
// value before the break and H the first heartbeat's declared value after
// reconnect. Both ends are filer-clock nanoseconds, matching object mtimes;
// taking min/max keeps the window oriented even if the filer clock rolled
// back in between (D-037 / storage-event-source report §0.4).
//
// from is not floored at zero: a margin larger than min(C,H) yields a
// negative bound, which for mtime comparisons simply means "no lower bound".
func GapWindow(c, h int64, margin time.Duration) (from, to int64) {
	lo, hi := c, h
	if lo > hi {
		lo, hi = hi, lo
	}
	m := margin.Nanoseconds()
	if m < 0 {
		m = 0
	}
	return lo - m, hi
}

// CursorBeforeEarliestLog reports whether cursor predates the earliest
// available metadata log file, i.e. whether the part of the log the cursor
// points into has already been purged by fs.log.purge (whole days at a time;
// the subscription silently skips such ranges, D-037 known constraints). A
// true result means "alert and reconcile", not "resume normally".
//
// earliestLogName is the earliest entry below /topics/.system/log/ as listed
// via the filer, either a day directory ("2026-10-15") or a minute file
// ("2026-10-15/14-23.0123abcd"). A leading /topics/.system/log/ prefix and
// surrounding slashes are tolerated; the optional filer-id suffix after the
// minute is ignored. Names are UTC.
//
// A cursor exactly equal to the file's own minute timestamp is still in
// range: the file is named by its window's start minute, so events at that
// timestamp live in that file.
func CursorBeforeEarliestLog(cursor int64, earliestLogName string) (bool, error) {
	name := strings.TrimSpace(earliestLogName)
	name = strings.TrimPrefix(name, systemLogDir)
	name = strings.Trim(name, "/")
	if name == "" {
		return false, ErrEmptyLogName
	}

	dayPart, filePart, hasFile := strings.Cut(name, "/")
	if strings.Contains(filePart, "/") {
		return false, fmt.Errorf("seaweedfs: unexpected extra path component in log name %q", earliestLogName)
	}

	day, err := parseDayDir(dayPart)
	if err != nil {
		return false, err
	}
	ts := day
	if hasFile {
		if filePart == "" {
			return false, fmt.Errorf("seaweedfs: empty minute file name in log name %q", earliestLogName)
		}
		minute, err := parseMinuteFile(filePart)
		if err != nil {
			return false, err
		}
		ts = day.Add(minute)
	}
	return cursor < ts.UnixNano(), nil
}

// parseDayDir parses a "YYYY-MM-DD" day directory into UTC midnight,
// rejecting values that time.Date would silently normalize (2026-13-01,
// 2026-02-30, unpadded forms).
func parseDayDir(dayPart string) (time.Time, error) {
	if len(dayPart) != len(dayDirFormat) {
		return time.Time{}, fmt.Errorf("seaweedfs: malformed day directory %q in log name", dayPart)
	}
	t, err := time.Parse(dayDirFormat, dayPart)
	if err != nil {
		return time.Time{}, fmt.Errorf("seaweedfs: malformed day directory %q in log name: %w", dayPart, err)
	}
	if t.Format(dayDirFormat) != dayPart {
		return time.Time{}, fmt.Errorf("seaweedfs: day directory %q does not denote a real date", dayPart)
	}
	return t, nil
}

// parseMinuteFile parses a "<HH-MM>[.<filer-id>]" minute file name into the
// offset from the day directory's midnight, validating hour/minute ranges and
// ignoring the filer-id suffix.
func parseMinuteFile(filePart string) (time.Duration, error) {
	m := minuteFileRe.FindStringSubmatch(filePart)
	if m == nil {
		return 0, fmt.Errorf("seaweedfs: malformed minute file name %q", filePart)
	}
	hm := m[1]
	hour, err := strconv.Atoi(hm[0:2])
	if err != nil {
		return 0, fmt.Errorf("seaweedfs: malformed minute file name %q: %w", filePart, err)
	}
	minute, err := strconv.Atoi(hm[3:5])
	if err != nil {
		return 0, fmt.Errorf("seaweedfs: malformed minute file name %q: %w", filePart, err)
	}
	if hour > 23 || minute > 59 {
		return 0, fmt.Errorf("seaweedfs: minute file name %q out of range", filePart)
	}
	return time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, nil
}
