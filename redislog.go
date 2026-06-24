package main

// redislog.go — structured rendering of the redis server log stream.
//
// Redis lines carry their own prefix: "pid:role date time.ms LEVEL message"
// (e.g. "1:M 16 Jun 2026 14:56:37.312 * Ready to accept connections"), where
// LEVEL is a single char: '.' debug, '-' verbose, '*' notice, '#' warning. Raw,
// every line lands at one undifferentiated level. redisLogWriter sits between
// the container and Wool: it parses each line with a declarative gortk LogSpec,
// drops the redundant timestamp (Wool stamps its own), keeps pid as a field,
// and emits at the mapped Wool level.

import (
	"bytes"
	"io"
	"strings"

	"github.com/codefly-dev/core/wool"
	"github.com/mind-build/gortk"
)

// redisLog parses the redis log prefix. Named captures pid/role/level/msg become
// Record fields; the single-char level maps to a canonical severity.
var redisLog = mustCompileLog(gortk.LogSpec{
	LineRegex:    `^(?P<pid>\d+):(?P<role>\w) \d+ \w+ \d+ \d+:\d+:\d+\.\d+ (?P<level>[.\-*#]) (?P<msg>.*)$`,
	LevelMap:     map[string]string{".": "debug", "-": "info", "*": "info", "#": "warn"},
	DefaultLevel: "info",
})

func mustCompileLog(s gortk.LogSpec) *gortk.LogParser {
	p, err := s.Compile()
	if err != nil {
		panic("redis redislog: " + err.Error())
	}
	return p
}

// redisLogWriter parses the redis log stream and re-emits each line through Wool
// at a severity-mapped level. It implements io.Writer so it can replace the raw
// logger handed to the runner.
type redisLogWriter struct {
	w   *wool.Wool
	buf []byte
}

func newRedisLogWriter(w *wool.Wool) *redisLogWriter {
	return &redisLogWriter{w: w}
}

var _ io.Writer = (*redisLogWriter)(nil)

// Write buffers incoming bytes and flushes complete (newline-terminated) lines;
// a partial trailing line is held until its newline arrives.
func (p *redisLogWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(p.buf[:i], "\r"))
		p.buf = p.buf[i+1:]
		p.emit(line)
	}
	return len(b), nil
}

// emit parses one redis log line and forwards it at the mapped Wool level. Lines
// that don't match the redis prefix (startup banner, version line) come back at
// info so nothing is silently dropped.
func (p *redisLogWriter) emit(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	rec := redisLog.Parse(line)
	msg, _ := rec.Fields["msg"].(string)

	var fields []*wool.LogField
	if pid, ok := rec.Fields["pid"].(string); ok && pid != "" {
		fields = append(fields, wool.Field("pid", pid))
	}
	p.logAt(woolLevel(rec.Level), msg, fields...)
}

func woolLevel(level string) wool.Loglevel {
	switch level {
	case "fatal":
		return wool.FATAL
	case "error":
		return wool.ERROR
	case "warn":
		return wool.WARN
	case "debug":
		return wool.DEBUG
	default:
		return wool.INFO
	}
}

func (p *redisLogWriter) logAt(level wool.Loglevel, msg string, fields ...*wool.LogField) {
	switch level {
	case wool.FATAL:
		p.w.Fatal(msg, fields...)
	case wool.ERROR:
		p.w.Error(msg, fields...)
	case wool.WARN:
		p.w.Warn(msg, fields...)
	case wool.DEBUG:
		p.w.Debug(msg, fields...)
	default:
		p.w.Info(msg, fields...)
	}
}
