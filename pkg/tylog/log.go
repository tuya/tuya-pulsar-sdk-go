package tylog

import (
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tuya/tuya-pulsar-sdk-go/pkg/tyutils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

var log *zap.Logger

var sugarLog *zap.SugaredLogger

func SetGlobalLog(appName string, prod bool, opts ...OptionFunc) {
	if appName == "" {
		panic("appName is not allowed to be empty")
	}
	var config *LogConfig
	if prod {
		config = ProdLogConfig(appName)
	} else {
		config = DefaultLogConfig()
		config.appName = appName
	}

	for _, v := range opts {
		v(config)
	}

	log = NewLog(config)
	hook := NewLogrusToZapHook(log)
	logrus.AddHook(hook)
	logrus.SetLevel(ZapToLogrusLevel(config.level.Level()))
	logrus.SetFormatter(&logrus.JSONFormatter{})
	sugarLog = log.Sugar()
}

func NewLog(config *LogConfig) *zap.Logger {
	if !tyutils.IsDir(config.dir) {
		err := tyutils.Mkdir(config.dir)
		if err != nil {
			panic("mkdir logs failed")
		}
	}
	filePath := config.dir + "/" + config.appName + ".log"

	jLoger := &lumberjack.Logger{
		Filename:   filePath,
		MaxSize:    config.maxSize,
		MaxBackups: config.maxBackups,
	}
	jLoger.LocalTime = config.localTime

	// Wrap the file writer with BufferedWriteSyncer to avoid one sync write per log entry on hot paths.
	// 256 KB / 200 ms: trade-off between throughput and log loss on crash (at most 200 ms of logs lost).
	fileWriter := zapcore.WriteSyncer(&zapcore.BufferedWriteSyncer{
		WS:            zapcore.AddSync(jLoger),
		Size:          256 * 1024,
		FlushInterval: 200 * time.Millisecond,
	})
	var writer zapcore.WriteSyncer
	if config.multiWrite {
		// also write to stderr
		w2, closeOut, err := zap.Open([]string{"stderr"}...)
		if err != nil {
			if closeOut != nil {
				closeOut()
			}
			panic(err)
		}
		writer = zapcore.NewMultiWriteSyncer(fileWriter, w2)
	} else {
		writer = fileWriter
	}

	// encoder
	encConfig := zap.NewProductionEncoderConfig()
	encConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	var enc zapcore.Encoder
	if config.format == FormatEnumConsole {
		enc = zapcore.NewConsoleEncoder(encConfig)
	} else {
		enc = zapcore.NewJSONEncoder(encConfig)
	}

	// core
	core := zapcore.NewCore(
		enc,
		writer,
		config.level,
	)

	l := zap.New(core)
	l = l.WithOptions(zap.AddCaller(), zap.AddCallerSkip(1), zap.AddStacktrace(zapcore.ErrorLevel))

	// hook
	if len(config.Hooks) > 0 {
		hks := make([]zap.Option, 0, len(config.Hooks))
		for _, v := range config.Hooks {
			hk := zap.Hooks(v.DoHook)
			hks = append(hks, hk)

		}
		l = l.WithOptions(hks...)
	}

	RotatePeriod(jLoger, config.rotatePeriodSecond)
	return l
}

func DefaultLogConfig() *LogConfig {
	return &LogConfig{
		localTime:          true,
		multiWrite:         false,
		maxSize:            100,
		rotatePeriodSecond: DefaultRotatePeriodSecond,
		level:              LevelEnumDebug,
		appName:            "tmp",
		dir:                "logs",
		format:             FormatEnumConsole,
	}
}

func ProdLogConfig(appName string) *LogConfig {
	c := DefaultLogConfig()
	c.multiWrite = false
	c.level = LevelEnumInfo
	c.appName = appName
	c.format = FormatEnumJSON
	return c
}

func RotatePeriod(l *lumberjack.Logger, periodSecond int64) {
	now := time.Now().Unix()
	tickSecond := periodSecond - now%periodSecond
	tk := time.NewTicker(time.Duration(tickSecond) * time.Second)
	go func() {
		<-tk.C
		err := l.Rotate()
		if err != nil {
			Error("rotate failed", ErrorField(err))
		}
		tk.Stop()
		periodSecondTicker := time.NewTicker(time.Duration(periodSecond) * time.Second)
		for range periodSecondTicker.C {
			err := l.Rotate()
			if err != nil {
				Error("rotate failed", ErrorField(err))
			}
		}
	}()
}

// Debug logs a message at DebugLevel.
func Debug(msg string, fields ...zap.Field) {
	log.Debug(msg, fields...)
}

// Info logs a message at InfoLevel.
func Info(msg string, fields ...zap.Field) {
	log.Info(msg, fields...)
}

// Warn logs a message at WarnLevel.
func Warn(msg string, fields ...zap.Field) {
	log.Warn(msg, fields...)
}

// Error logs a message at ErrorLevel.
func Error(msg string, fields ...zap.Field) {
	log.Error(msg, fields...)
}

// Fatal logs a message at ErrorLevel and then calls os.Exit(1).
func Fatal(msg string, fields ...zap.Field) {
	log.Error(msg, fields...)
}
