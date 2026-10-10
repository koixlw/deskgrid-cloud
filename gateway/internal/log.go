package internal

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

var (
	infoLog  *log.Logger
	errorLog *log.Logger
)

func InitLogger(logDir string) {
	_ = os.MkdirAll(logDir, 0o755)
	ts := time.Now().Format("20060102")
	gf, _ := os.OpenFile(filepath.Join(logDir, "gateway-"+ts+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	ef, _ := os.OpenFile(filepath.Join(logDir, "gateway-err-"+ts+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	infoLog = log.New(ioMulti(os.Stdout, gf), "[INFO] ", log.LstdFlags|log.Lmicroseconds)
	errorLog = log.New(ioMulti(os.Stderr, ef), "[ERROR] ", log.LstdFlags|log.Lmicroseconds)
}

type multiWriter struct {
	writers []interface{ Write([]byte) (int, error) }
}

func (m *multiWriter) Write(p []byte) (int, error) {
	for _, w := range m.writers {
		w.Write(p)
	}
	return len(p), nil
}

func ioMulti(writers ...interface{ Write([]byte) (int, error) }) *multiWriter {
	return &multiWriter{writers: writers}
}

func Info(format string, args ...interface{}) {
	if infoLog != nil {
		infoLog.Output(2, fmt.Sprintf(format, args...))
	}
}

func Errorf(format string, args ...interface{}) {
	if errorLog != nil {
		errorLog.Output(2, fmt.Sprintf(format, args...))
	}
}
