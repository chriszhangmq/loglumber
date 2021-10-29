package lumberjack

import (
	"log"
)

// To use lumberjack with the standard library's log package, just pass it into
// the SetOutput function when your application starts.
func Example() {
	log.SetOutput(&Logger{
		fullPathFileName:   "/var/log/myapp/foo.log",
		LogMaxSize:         500, // megabytes
		LogMaxSaveQuantity: 3,
		LogMaxSaveDay:      28,   // days
		Compress:           true, // disabled by default
	})
}
