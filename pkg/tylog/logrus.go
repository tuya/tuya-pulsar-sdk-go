package tylog

import (
	"github.com/sirupsen/logrus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LogrusToZapHook 是一个 logrus Hook，用于将 logrus 日志重定向到 zap
type LogrusToZapHook struct {
	zapLogger *zap.Logger
}

// NewLogrusToZapHook 创建一个新的 LogrusToZapHook
func NewLogrusToZapHook(zapLogger *zap.Logger) *LogrusToZapHook {
	return &LogrusToZapHook{
		zapLogger: zapLogger,
	}
}

// Levels 返回 logrus 支持的所有日志级别
func (h *LogrusToZapHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// Fire 将 logrus 的日志条目重定向到 zap
func (h *LogrusToZapHook) Fire(entry *logrus.Entry) error {
	var zapLevel zapcore.Level
	switch entry.Level {
	case logrus.DebugLevel:
		zapLevel = zapcore.DebugLevel
	case logrus.InfoLevel:
		zapLevel = zapcore.InfoLevel
	case logrus.WarnLevel:
		zapLevel = zapcore.WarnLevel
	case logrus.ErrorLevel:
		zapLevel = zapcore.ErrorLevel
	case logrus.FatalLevel:
		zapLevel = zapcore.FatalLevel
	case logrus.PanicLevel:
		zapLevel = zapcore.DPanicLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	h.zapLogger.With(
		zap.String("source", "logrus"),
		zap.String("msg", entry.Message),
	).Check(zapLevel, entry.Message).Write()

	return nil
}

func ZapToLogrusLevel(zapLevel zapcore.Level) logrus.Level {
	switch zapLevel {
	case zapcore.DebugLevel:
		return logrus.DebugLevel
	case zapcore.InfoLevel:
		return logrus.InfoLevel
	case zapcore.WarnLevel:
		return logrus.WarnLevel
	case zapcore.ErrorLevel:
		return logrus.ErrorLevel
	case zapcore.DPanicLevel, zapcore.PanicLevel:
		return logrus.PanicLevel
	case zapcore.FatalLevel:
		return logrus.FatalLevel
	default:
		// 默认情况下返回 logrus 的 InfoLevel
		return logrus.InfoLevel
	}
}
