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