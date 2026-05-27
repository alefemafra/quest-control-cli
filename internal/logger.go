package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type MissionLogger struct {
	dir          string
	orchestrator *os.File
	featureLogs  map[string]*os.File
	entries      []LogEntry
	mu           sync.Mutex
}

func NewMissionLogger(missionDir string) (*MissionLogger, error) {
	logDir := filepath.Join(missionDir, "logs")
	legacyLogDir := filepath.Join(missionDir, "mission", "logs")
	if fileExists(legacyLogDir) && !fileExists(logDir) {
		logDir = legacyLogDir
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(
		filepath.Join(logDir, "orchestrator.log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o644,
	)
	if err != nil {
		return nil, err
	}

	return &MissionLogger{
		dir:          logDir,
		orchestrator: f,
		featureLogs:  make(map[string]*os.File),
	}, nil
}

func (l *MissionLogger) Log(featureID, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	ts := time.Now()
	text := fmt.Sprintf(format, args...)
	tsStr := ts.Format("15:04:05")

	l.entries = append(l.entries, LogEntry{
		Time:      tsStr,
		FeatureID: featureID,
		Text:      text,
	})

	line := fmt.Sprintf("[%s] %s\n", tsStr, text)

	if featureID != "" {
		fmt.Fprintf(l.orchestrator, "[%s] [%s] %s\n", tsStr, featureID, text)
	} else {
		fmt.Fprintf(l.orchestrator, "[%s] [ORCH] %s\n", tsStr, text)
	}

	if featureID != "" {
		f := l.featureLog(featureID)
		if f != nil {
			fmt.Fprint(f, line)
		}
	}
}

func (l *MissionLogger) featureLog(id string) *os.File {
	f, ok := l.featureLogs[id]
	if ok {
		return f
	}
	f, err := os.OpenFile(
		filepath.Join(l.dir, id+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o644,
	)
	if err != nil {
		return nil
	}
	l.featureLogs[id] = f
	return f
}

func (l *MissionLogger) Entries() []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]LogEntry, len(l.entries))
	copy(cp, l.entries)
	return cp
}

func (l *MissionLogger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.orchestrator != nil {
		l.orchestrator.Close()
	}
	for _, f := range l.featureLogs {
		f.Close()
	}
}
