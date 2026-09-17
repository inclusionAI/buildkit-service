package buildbatch

import (
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"time"
)

var commandStartUnixNano atomic.Int64

var logProgressState struct {
	current atomic.Int64
	total   atomic.Int64
}

func resetLogProgress(total int) {
	logProgressState.current.Store(0)
	logProgressState.total.Store(int64(total))
}

func advanceLogProgress() int64 {
	return logProgressState.current.Add(1)
}

func clearLogProgress() {
	logProgressState.current.Store(0)
	logProgressState.total.Store(0)
}

func resetCommandStartTime(start time.Time) {
	commandStartUnixNano.Store(start.UnixNano())
}

func currentCommandStartTime() time.Time {
	unixNano := commandStartUnixNano.Load()
	if unixNano == 0 {
		return time.Now()
	}
	return time.Unix(0, unixNano)
}

func formatLogElapsed(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(100 * time.Millisecond).String()
}

func formatElapsed(d time.Duration) string {
	s := roundElapsedSeconds(d)
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	m := int(s) / 60
	rs := s - float64(m*60)
	return fmt.Sprintf("%dm%.1fs", m, rs)
}

func roundElapsedSeconds(d time.Duration) float64 {
	return math.Round(d.Seconds()*10) / 10
}

func logInfo(format string, args ...any) {
	logWithLevel("INFO", format, args...)
}

// LogInfo writes an informational message using the batch command's log format.
func LogInfo(format string, args ...any) {
	logInfo(format, args...)
}

func logError(format string, args ...any) {
	logWithLevel("ERROR", format, args...)
}

// LogError writes an error message using the batch command's log format.
func LogError(format string, args ...any) {
	logError(format, args...)
}

// ResetCommandStartTime sets the origin used for elapsed-time logging.
func ResetCommandStartTime(start time.Time) {
	resetCommandStartTime(start)
}

func logWithLevel(level string, format string, args ...any) {
	now := time.Now().Format(time.RFC3339)
	elapsed := formatLogElapsed(time.Since(currentCommandStartTime()))
	current := logProgressState.current.Load()
	total := logProgressState.total.Load()
	prefix := fmt.Sprintf("[%s] [%s] [%d/%d] [%s] ", now, elapsed, current, total, level)
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}
