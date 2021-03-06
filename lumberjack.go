// Package lumberjack provides a rolling logger.
//
// Note that this is v2.0 of lumberjack, and should be imported using gopkg.in
// thusly:
//
//   import "gopkg.in/natefinch/lumberjack.v2"
//
// The package name remains simply lumberjack, and the code resides at
// https://github.com/natefinch/lumberjack under the v2.0 branch.
//
// Lumberjack is intended to be one part of a logging infrastructure.
// It is not an all-in-one solution, but instead is a pluggable
// component at the bottom of the logging stack that simply controls the files
// to which logs are written.
//
// Lumberjack plays well with any logging package that can write to an
// io.Writer, including the standard library's log package.
//
// Lumberjack assumes that only one process is writing to the output files.
// Using the same lumberjack configuration from multiple processes on the same
// machine will result in improper behavior.
package lumberjack

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	dateFormat = "2006-01-02"
	timeFormat = "2006-01-02_15:04:05"
	//压缩文件名的时间格式
	backupTimeFormat = "2006-01-02T15-04-05"
	compressSuffix   = ".gz"
	//默认1PB分割一次(默认：永不按照文件大小分割)
	defaultMaxSize = 1024 * 1024 * 1024
)

// ensure we always implement io.WriteCloser
var _ io.WriteCloser = (*Logger)(nil)

// Logger is an io.WriteCloser that writes to the specified filename.
//
// Logger opens or creates the logfile on first Write.  If the file exists and
// is less than LogMaxSize megabytes, lumberjack will open and append to that file.
// If the file exists and its size is >= LogMaxSize megabytes, the file is renamed
// by putting the current time in a timestamp in the name immediately before the
// file's extension (or the end of the filename if there's no extension). A new
// log file is then created using original filename.
//
// Whenever a write would cause the current log file exceed LogMaxSize megabytes,
// the current file is closed, renamed, and a new log file created with the
// original name. Thus, the filename you give Logger is always the "current" log
// file.
//
// Backups use the log file name given to Logger, in the form
// `name-timestamp.ext` where name is the filename without the extension,
// timestamp is the time at which the log was rotated formatted with the
// time.Time format of `2006-01-02T15-04-05.000` and the extension is the
// original extension.  For example, if your Logger.fullPathFileName is
// `/var/log/foo/server.log`, a backup created at 6:30pm on Nov 11 2016 would
// use the filename `/var/log/foo/server-2016-11-04T18-30-00.000.log`
//
// Cleaning Up Old Log Files
//
// Whenever a new logfile gets created, old log files may be deleted.  The most
// recent files according to the encoded timestamp will be retained, up to a
// number equal to LogMaxSaveQuantity (or all of them if LogMaxSaveQuantity is 0).  Any files
// with an encoded timestamp older than LogMaxSaveDay days are deleted, regardless of
// LogMaxSaveQuantity.  Note that the time encoded in the timestamp is the rotation
// time, which may differ from the last time that file was written to.
//
// If LogMaxSaveQuantity and LogMaxSaveDay are both 0, no old log files will be deleted.
type Logger struct {
	// fullPathFileName is the file to write logs to.  Backup log files will be retained
	// in the same directory.  It uses <processname>-lumberjack.log in
	// os.TempDir() if empty.

	// LogMaxSize is the maximum size in megabytes of the log file before it gets
	// rotated. It defaults to 100 megabytes.
	LogMaxSize int `json:"LogMaxSize" yaml:"LogMaxSize"`

	// LogMaxSaveDay is the maximum number of days to retain old log files based on the
	// timestamp encoded in their filename.  Note that a day is defined as 24
	// hours and may not exactly correspond to calendar days due to daylight
	// savings, leap seconds, etc. The default is not to remove old log files
	// based on age.
	LogMaxSaveDay int `json:"LogMaxSaveDay" yaml:"LogMaxSaveDay"`

	// LogMaxSaveQuantity is the maximum number of old log files to retain.  The default
	// is to retain all old log files (though LogMaxSaveDay may still cause them to get
	// deleted.)
	LogMaxSaveQuantity int `json:"LogMaxSaveQuantity" yaml:"LogMaxSaveQuantity"`

	// LocalTime determines if the time used for formatting the timestamps in
	// backup files is the computer's local time.  The default is to use UTC
	// time.
	LocalTime bool `json:"LocalTime" yaml:"LocalTime"`

	// Compress determines if the rotated log files should be compressed
	// using gzip. The default is not to perform compression.
	Compress bool `json:"Compress" yaml:"Compress"`

	//日志分割单位：天
	LogSplitDay int `json:"LogSplitDay" yaml:"LogSplitDay"`

	//日志保存路径
	LogPathName string `json:"LogPathName" yaml:"LogPathName"`

	//日志名称
	LogFileName string `json:"LogFileName" yaml:"LogFileName"`

	//日志后缀
	LogFileSuffix string `json:"LogFileSuffix" yaml:"LogFileSuffix"`

	//日志中的时间格式
	LogFileTimeFormat string `json:"LogFileTimeFormat" yaml:"LogFileTimeFormat"`

	//统计过了几天：是否到达需要分割日志的时候
	splitDayCount int
	//全路径的日志名
	fullPathFileName string

	size int64
	file *os.File
	mu   sync.Mutex

	millCh    chan bool
	startMill sync.Once
}

var (
	// currentTime exists so it can be mocked out by tests.
	currentTime = time.Now

	// os_Stat exists so it can be mocked out by tests.
	osStat = os.Stat

	// megabyte is the conversion factor between LogMaxSize and bytes.  It is a
	// variable so tests can mock it out and not need to write megabytes of data
	// to disk.
	megabyte = 1024 * 1024

	//当前时间
	nowTime time.Time
	//当前时间戳
	nowTimestamp int64
	//当天的23时59分时间戳
	lastTimestamp int64
	//昨天的23时59分时间戳
	yesterdayLastTimestamp int64
	//执行按天分割操作
	isSplitDay bool
)

func (l *Logger) Init() {
	updateCurrentTimestamp(l.LocalTime)
	updateLastTimeOfToday(l.LocalTime)
	updateYesterdayTime(l.LocalTime)
	l.fullPathFileName = l.LogPathName + l.LogFileName + l.LogFileSuffix
	isSplitDay = false
	//若日志文件并非当天的，则执行打包命令
	isExist, err := pathFileExist(l.fullPathFileName)
	if err != nil {
		panic(err)
	}
	if isExist {
		//获取日志更新时间
		logFileUpdateTime := getLogFileUpdateTime(l.fullPathFileName)
		//仅当日志文件的最后一条记录时间 <= 昨天23:29:59，才执行文件压缩
		if len(logFileUpdateTime) > 0 && l.strTime2TimeStamp(logFileUpdateTime) <= yesterdayLastTimestamp {
			//改名字
			newLogFileName := l.changeFileNameByTime(logFileUpdateTime)
			//启动时，处理需要上次推出程序未压缩的日志文件
			_ = l.compressFiles(newLogFileName)
			//启动时处理文件：压缩、删除
			_ = l.millRunOnce()
		}
	}
}

// Write implements io.Writer.  If a write would cause the log file to be larger
// than LogMaxSize, the file is closed, renamed to include a timestamp of the
// current time, and a new log file is created using the original log file name.
// If the length of the write is greater than LogMaxSize, an error is returned.
func (l *Logger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	writeLen := int64(len(p))
	if writeLen > l.max() {
		return 0, fmt.Errorf(
			"write length %d exceeds maximum file size %d", writeLen, l.max(),
		)
	}

	if l.file == nil {
		if err = l.openExistingOrNew(len(p)); err != nil {
			return 0, err
		}
	}

	//按天分割日志
	if l.LogSplitDay > 0 && isNextDay(l.LocalTime) {
		updateLastTimeOfToday(l.LocalTime)
		updateYesterdayTime(l.LocalTime)
		l.splitDayCount++
		//是否达到分割要求
		if l.LogSplitDay <= l.splitDayCount {
			l.splitDayCount = 0
			isSplitDay = true
			if err := l.rotate(); err != nil {
				return 0, err
			}
		}
		isSplitDay = false
	}

	//超过单个文件大小：压缩该文件
	if l.size+writeLen > l.max() {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = l.file.Write(p)
	l.size += int64(n)

	return n, err
}

// Close implements io.Closer, and closes the current logfile.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.close()
}

// close closes the file if it is open.
func (l *Logger) close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Rotate causes Logger to close the existing log file and immediately create a
// new one.  This is a helper function for applications that want to initiate
// rotations outside of the normal rotation rules, such as in response to
// SIGHUP.  After rotating, this initiates compression and removal of old log
// files according to the configuration.
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotate()
}

// rotate closes the current file, moves it aside with a timestamp in the name,
// (if it exists), opens a new file with the original filename, and then runs
// post-rotation processing and removal.
func (l *Logger) rotate() error {
	if err := l.close(); err != nil {
		return err
	}
	if err := l.openNew(); err != nil {
		return err
	}
	l.mill()
	return nil
}

// openNew opens a new log file for writing, moving any old log file out of the
// way.  This methods assumes the file has already been closed.
func (l *Logger) openNew() error {
	err := os.MkdirAll(l.dir(), 0755)
	if err != nil {
		return fmt.Errorf("can't make directories for new logfile: %s", err)
	}

	name := l.filename()
	mode := os.FileMode(0600)
	info, err := osStat(name)
	if err == nil {
		// Copy the mode off the old logfile.
		mode = info.Mode()
		// move the existing file
		newname := backupName(name, l.LocalTime)
		if err := os.Rename(name, newname); err != nil {
			return fmt.Errorf("can't rename log file: %s", err)
		}

		// this is a no-op anywhere but linux
		if err := chown(name, info); err != nil {
			return err
		}
	}

	// we use truncate here because this should only get called when we've moved
	// the file ourselves. if someone else creates the file in the meantime,
	// just wipe out the contents.
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("can't open new logfile: %s", err)
	}
	l.file = f
	l.size = 0
	return nil
}

// backupName creates a new filename from the given name, inserting a timestamp
// between the filename and the extension, using the local time if requested
// (otherwise UTC).
func backupName(name string, local bool) string {
	var timestamp string
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	t := currentTime()
	if !local {
		t = t.UTC()
	}
	if isSplitDay {
		timestamp = time.Unix(yesterdayLastTimestamp, 0).Format(backupTimeFormat)
	} else {
		timestamp = t.Format(backupTimeFormat)
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestamp, ext))
}

// openExistingOrNew opens the logfile if it exists and if the current write
// would not put it over LogMaxSize.  If there is no such file or the write would
// put it over the LogMaxSize, a new file is created.
func (l *Logger) openExistingOrNew(writeLen int) error {
	l.mill()

	filename := l.filename()
	info, err := osStat(filename)
	if os.IsNotExist(err) {
		return l.openNew()
	}
	if err != nil {
		return fmt.Errorf("error getting log file info: %s", err)
	}

	if info.Size()+int64(writeLen) >= l.max() {
		return l.rotate()
	}

	file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// if we fail to open the old log file for some reason, just ignore
		// it and open a new log file.
		return l.openNew()
	}
	l.file = file
	l.size = info.Size()
	return nil
}

// filename generates the name of the logfile from the current time.
func (l *Logger) filename() string {
	if l.fullPathFileName != "" {
		return l.fullPathFileName
	}
	name := filepath.Base(os.Args[0]) + "-lumberjack.log"
	return filepath.Join(os.TempDir(), name)
}

// millRunOnce performs compression and removal of stale log files.
// Log files are compressed if enabled via configuration and old log
// files are removed, keeping at most l.LogMaxSaveQuantity files, as long as
// none of them are older than LogMaxSaveDay.
func (l *Logger) millRunOnce() error {
	if l.LogMaxSaveQuantity == 0 && l.LogMaxSaveDay == 0 && !l.Compress {
		return nil
	}

	files, err := l.oldLogFiles()
	if err != nil {
		return err
	}

	var compress, remove []logInfo

	if l.LogMaxSaveQuantity > 0 && l.LogMaxSaveQuantity < len(files) {
		preserved := make(map[string]bool)
		var remaining []logInfo
		for _, f := range files {
			// Only count the uncompressed log file or the
			// compressed log file, not both.
			fn := f.Name()
			if strings.HasSuffix(fn, compressSuffix) {
				fn = fn[:len(fn)-len(compressSuffix)]
			}
			preserved[fn] = true

			if len(preserved) > l.LogMaxSaveQuantity {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}
	if l.LogMaxSaveDay > 0 {
		diff := time.Duration(int64(24*time.Hour) * int64(l.LogMaxSaveDay))
		updateCurrentTimestamp(l.LocalTime)
		cutoff := nowTime.Add(-1 * diff)

		var remaining []logInfo
		for _, f := range files {
			if f.timestamp.Unix() < cutoff.Unix() {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}

	if l.Compress {
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), compressSuffix) {
				compress = append(compress, f)
			}
		}
	}

	for _, f := range remove {
		errRemove := os.Remove(filepath.Join(l.dir(), f.Name()))
		if err == nil && errRemove != nil {
			err = errRemove
		}
	}
	for _, f := range compress {
		fn := filepath.Join(l.dir(), f.Name())
		errCompress := compressLogFile(fn, fn+compressSuffix)
		if err == nil && errCompress != nil {
			err = errCompress
		}
	}

	return err
}

// millRun runs in a goroutine to manage post-rotation compression and removal
// of old log files.
func (l *Logger) millRun() {
	for range l.millCh {
		// what am I going to do, log this?
		_ = l.millRunOnce()
	}
}

// mill performs post-rotation compression and removal of stale log files,
// starting the mill goroutine if necessary.
func (l *Logger) mill() {
	l.startMill.Do(func() {
		l.millCh = make(chan bool, 1)
		go l.millRun()
	})
	select {
	case l.millCh <- true:
	default:
	}
}

// oldLogFiles returns the list of backup log files stored in the same
// directory as the current log file, sorted by ModTime
func (l *Logger) oldLogFiles() ([]logInfo, error) {
	files, err := ioutil.ReadDir(l.dir())
	if err != nil {
		return nil, fmt.Errorf("can't read log file directory: %s", err)
	}
	logFiles := []logInfo{}

	prefix, ext := l.prefixAndExt()

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if t, err := l.timeFromName(f.Name(), prefix, ext); err == nil {
			logFiles = append(logFiles, logInfo{t, f})
			continue
		}
		if t, err := l.timeFromName(f.Name(), prefix, ext+compressSuffix); err == nil {
			logFiles = append(logFiles, logInfo{t, f})
			continue
		}
		// error parsing means that the suffix at the end was not generated
		// by lumberjack, and therefore it's not a backup file.
	}

	sort.Sort(byFormatTime(logFiles))

	return logFiles, nil
}

// timeFromName extracts the formatted time from the filename by stripping off
// the filename's prefix and extension. This prevents someone's filename from
// confusing time.parse.
func (l *Logger) timeFromName(filename, prefix, ext string) (time.Time, error) {
	if !strings.HasPrefix(filename, prefix) {
		return time.Time{}, errors.New("mismatched prefix")
	}
	if !strings.HasSuffix(filename, ext) {
		return time.Time{}, errors.New("mismatched extension")
	}
	ts := filename[len(prefix) : len(filename)-len(ext)]
	if l.LocalTime {
		return time.ParseInLocation(backupTimeFormat, ts, time.Local)
	}
	return time.Parse(backupTimeFormat, ts)
}

// max returns the maximum size in bytes of log files before rolling.
func (l *Logger) max() int64 {
	if l.LogMaxSize == 0 {
		return int64(defaultMaxSize * megabyte)
	}
	return int64(l.LogMaxSize) * int64(megabyte)
}

// dir returns the directory for the current filename.
func (l *Logger) dir() string {
	return filepath.Dir(l.filename())
}

// prefixAndExt returns the filename part and extension part from the Logger's
// filename.
func (l *Logger) prefixAndExt() (prefix, ext string) {
	filename := filepath.Base(l.filename())
	ext = filepath.Ext(filename)
	prefix = filename[:len(filename)-len(ext)] + "-"
	return prefix, ext
}

// compressLogFile compresses the given log file, removing the
// uncompressed log file if successful.
func compressLogFile(src, dst string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer f.Close()

	fi, err := osStat(src)
	if err != nil {
		return fmt.Errorf("failed to stat log file: %v", err)
	}

	if err := chown(dst, fi); err != nil {
		return fmt.Errorf("failed to chown compressed log file: %v", err)
	}

	// If this file already exists, we presume it was created by
	// a previous attempt to compress the log file.
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %v", err)
	}
	defer gzf.Close()

	gz := gzip.NewWriter(gzf)

	defer func() {
		if err != nil {
			os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %v", err)
		}
	}()

	if _, err := io.Copy(gz, f); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := gzf.Close(); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}

	return nil
}

// logInfo is a convenience struct to return the filename and its embedded
// timestamp.
type logInfo struct {
	timestamp time.Time
	os.FileInfo
}

// byFormatTime sorts by newest time formatted in the name.
type byFormatTime []logInfo

func (b byFormatTime) Less(i, j int) bool {
	return b[i].timestamp.After(b[j].timestamp)
}

func (b byFormatTime) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byFormatTime) Len() int {
	return len(b)
}

//更新当天的23时59分时间戳
func updateLastTimeOfToday(local bool) {
	currTime := time.Unix(nowTimestamp, 0)
	endDate := currTime.Format(dateFormat) + "_23:59:59"
	if !local {
		//UTC
		endTimeStamp, _ := time.Parse(timeFormat, endDate)
		lastTimestamp = endTimeStamp.Unix()
	} else {
		//local
		endTimeStamp, _ := time.ParseInLocation(timeFormat, endDate, time.Local)
		lastTimestamp = endTimeStamp.Unix()
	}
}

func updateYesterdayTime(local bool) {
	yesterdayTime := time.Unix(nowTimestamp, 0).AddDate(0, 0, -1)
	yesterdayLastTime := yesterdayTime.Format(dateFormat) + "_23:59:59"
	if !local {
		//UTC
		endTimeStamp, _ := time.Parse(timeFormat, yesterdayLastTime)
		yesterdayLastTimestamp = endTimeStamp.Unix()
	} else {
		//local
		endTimeStamp, _ := time.ParseInLocation(timeFormat, yesterdayLastTime, time.Local)
		yesterdayLastTimestamp = endTimeStamp.Unix()
	}
}

//更新当前时间戳
func updateCurrentTimestamp(local bool) {
	t := currentTime()
	if !local {
		t = t.UTC()
	}
	nowTime = t
	nowTimestamp = t.Unix()
}

//当前时间是否超过0点（进入下一天）
func isNextDay(local bool) bool {
	updateCurrentTimestamp(local)
	return nowTimestamp > lastTimestamp
}

//读取日志文件非空的最后一行，并获取时间
func getLogFileUpdateTime(filePath string) string {
	//读取最后一行
	lastLine := getLastLineWithSeek(filePath)
	//获取该行中的时间
	lastTime := getTimeFromStr(lastLine)
	return lastTime
}

func getTimeFromStr(str string) string {
	planRegx := regexp.MustCompile("([0-9]|[ ]|[-]|[:])+")
	subs := planRegx.FindStringSubmatch(str)
	if len(subs) > 0 {
		return strings.TrimSpace(subs[0])
	}
	return ""
}

func getLastLineWithSeek(filepath string) string {
	fileHandle, err := os.Open(filepath)
	if err != nil {
		panic("Cannot open file")
	}
	defer fileHandle.Close()
	var line string
	var cursor int64 = 0
	stat, _ := fileHandle.Stat()
	fileSize := stat.Size()
	for fileSize > 0 {
		cursor -= 1
		if _, err := fileHandle.Seek(cursor, io.SeekEnd); err != nil {
			panic(err)
		}
		char := make([]byte, 1)
		if _, err := fileHandle.Read(char); err != nil {
			panic(err)
		}
		//是否为非空的倒数第一行
		if cursor != -1 && (char[0] == '\n' || char[0] == '\r') && !strIsNull(line) {
			break
		}
		line = string(char) + line
		//遍历到文件开头
		if cursor == -fileSize {
			break
		}
	}
	//返回非空的倒数第一行
	return strings.TrimSpace(line)
}

func strIsNull(line string) bool {
	temp := strings.TrimSpace(line)
	return len(temp) <= 0 || temp == ""
}

func pathFileExist(filePath string) (bool, error) {
	_, err := os.Stat(filePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (l *Logger) changeFileNameByTime(lastTime string) string {
	var newFileTime time.Time
	var err error
	//时间字符串 =》 当前字符串的时间格式
	if l.LocalTime {
		newFileTime, err = time.ParseInLocation(l.LogFileTimeFormat, lastTime, time.Local)
	} else {
		newFileTime, err = time.Parse(l.LogFileTimeFormat, lastTime)
	}
	if err != nil {
		log.Fatal(err)
	}
	//当前字符串的时间格式 =》 时间戳 =》 log文件的时间格式
	newFileTimestamp := newFileTime.Unix()
	//新文件名
	newFileName := l.LogFileName + "-" + time.Unix(newFileTimestamp, 0).Format(backupTimeFormat)
	//更改文件名
	l.changeFileName(l.LogPathName, l.LogFileName+l.LogFileSuffix, newFileName+l.LogFileSuffix)
	return newFileName + l.LogFileSuffix
}

func (l *Logger) changeFileName(pathName string, odlFileName string, newFileName string) {
	err := os.Rename(path.Join(pathName, odlFileName), path.Join(pathName, newFileName))
	if err != nil {
		panic(err)
	}
}

//时间字符串 =》 当前字符串的时间格式的时间戳
func (l *Logger) strTime2TimeStamp(strTime string) int64 {
	var err error
	var tmpTime time.Time
	if l.LocalTime {
		tmpTime, err = time.ParseInLocation(l.LogFileTimeFormat, strTime, time.Local)
	} else {
		tmpTime, err = time.Parse(l.LogFileTimeFormat, strTime)
	}
	if err != nil {
		log.Fatal(err)
	}
	return tmpTime.Unix()
}

func (l *Logger) compressFiles(fileName string) error {
	files, err := l.oldLogFiles()
	if err != nil {
		return err
	}

	var remaining logInfo

	if l.LogMaxSaveDay > 0 {
		diff := time.Duration(int64(24*time.Hour) * int64(l.LogMaxSaveDay))
		updateCurrentTimestamp(l.LocalTime)
		cutoff := nowTime.Add(-1 * diff)
		for _, f := range files {
			if f.Name() == fileName && f.timestamp.Unix() > cutoff.Unix() {
				remaining = f
				break
			}
		}
	}

	if l.Compress {
		//当前文件需要压缩
		if !reflect.DeepEqual(remaining, logInfo{}) && !strings.HasSuffix(remaining.Name(), compressSuffix) {
			//压缩
			fn := filepath.Join(l.dir(), remaining.Name())
			errCompress := compressLogFile(fn, fn+compressSuffix)
			if errCompress != nil {
				err = errCompress
			}
		}
	}

	return err
}
