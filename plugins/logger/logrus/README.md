# logrus

[logrus](https://github.com/sirupsen/logrus) logger implementation

## Usage

```go
 
l:=logger.New(logrus.New(
		logrus.WithFormatter(&log.TextFormatter{
			ForceColors:     true,
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02 15:04:05.000",
		}),
	), logger.WithLevel(logger.Debug))

l.Infof("testing: %s", "Infof")

```

## 注意：SetLevel / SetOutput 作用于全局实例

recorder 路径的 `SetLevel` / `SetOutput` 会直接作用于底层 `logrus.Logger` 全局实例
（`l.SetLevel(...)` / `l.SetOutput(...)`）。多个 logger 实例共享同一个
`logrus.Logger` 时会互相影响——后调用的 SetLevel/SetOutput 会覆盖先调用的结果。
**建议单实例使用**；如需多实例隔离，请分别为每个实例 `logrus.New()` 创建独立的
`logrus.Logger`。