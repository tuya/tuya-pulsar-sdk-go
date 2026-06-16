package tylog

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	FormatEnumJSON    FormatEnum = "json"
	FormatEnumConsole FormatEnum = "console"

	DefaultRotatePeriodSecond int64 = 60 * 60 * 24 // one day
)

var (
	LevelEnumDebug = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	LevelEnumInfo  = zap.NewAtomicLevelAt(zapcore.InfoLevel)
)

type LogConfig struct {
	// localTime: true uses local time in log filenames (recommended for CN deployments).
	localTime bool
	// multiWrite: when true, logs are written to both the file and stderr. Default false.
	// Enable with WithMultiWriteOption(true) to see terminal output.
	multiWrite bool
	// level: log level. Default DebugLevel.
	level LevelEnum
	// appName: default "tmp".
	appName string
	// dir: log file directory. Default "logs".
	dir string
	// format: default console (easier to read during development).
	format FormatEnum
	// hook
	Hooks []LogHook
	// maxSize: max size of a single log file in MB. Default 100.
	maxSize int
	// rotatePeriodSecond: log rotation interval. Default one day.
	rotatePeriodSecond int64
	// maxBackups: max number of rotated backup files. Zero means unlimited.
	maxBackups int
}

type LevelEnum = zap.AtomicLevel

type FormatEnum string

type FormatTime = zapcore.TimeEncoder
