package filecache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const VERSION = "1.0.2"

// File size constants for use with FileCache.MaxSize.
// For example, cache.MaxSize = 64 * Megabyte
const (
	Kilobyte = 1024               //单位KB
	Megabyte = 1024 * 1024        //单位MB
	Gigabyte = 1024 * 1024 * 1024 //单位GB
)

var (
	DefaultExpireItem int   = 300 // 5 minutes
	DefaultMaxSize    int64 = 16 * Megabyte
	DefaultMaxItems   int   = 32
	DefaultEvery      int   = 60 // 1 minute
)

var (
	InvalidCacheItem = errors.New("invalid cache item")
	ItemIsDirectory  = errors.New("can't cache a directory")
	ItemNotInCache   = errors.New("item not in cache")
	ItemTooLarge     = errors.New("item too large for cache")
	WriteIncomplete  = errors.New("incomplete write of cache item")
)

var SquelchItemNotInCache = true

// Mumber of items to buffer adding to the file cache.
//配置缓存池的大小，当前正在往缓存中添加的文件数。
var NewCachePipeSize = 4

//缓存项
type cacheItem struct {
	content    []byte     //缓存内容
	lock       sync.Mutex //锁
	Size       int64      //内容大小
	Lastaccess time.Time  //最近访问时间
	Modified   time.Time  //最近更新时间
}

//缓存是否被更新
func (itm *cacheItem) WasModified(fi os.FileInfo) bool {
	itm.lock.Lock()
	defer itm.lock.Unlock()
	return itm.Modified.Equal(fi.ModTime())
}

//获取缓存的内容
func (itm *cacheItem) GetReader() io.Reader {
	b := bytes.NewReader(itm.Access())
	return b
}

//访问缓存的内容
func (itm *cacheItem) Access() []byte {
	itm.lock.Lock()
	defer itm.lock.Unlock()
	itm.Lastaccess = time.Now()
	return itm.content
}

//获取缓存上次访问时间的间隔
func (itm *cacheItem) Dur() time.Duration {
	itm.lock.Lock()
	defer itm.lock.Unlock()
	return time.Now().Sub(itm.Lastaccess)
}

//缓存文件
func cacheFile(path string, maxSize int64) (itm *cacheItem, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	} else if fi.Mode().IsDir() {
		return nil, ItemIsDirectory
	} else if fi.Size() > maxSize {
		return nil, ItemTooLarge
	}

	content, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}

	itm = &cacheItem{
		content:    content,
		Size:       fi.Size(),
		Modified:   fi.ModTime(),
		Lastaccess: time.Now(),
	}
	return
}

// FileCache represents a cache in memory.
// An ExpireItem value of 0 means that items should not be expired based
// on time in memory.
//代表内存中的缓存，属性ExpireItem值为0表示改项目在内存中没有过期时间限制。
type FileCache struct {
	dur        time.Duration
	items      map[string]*cacheItem
	in         chan string
	mutex      sync.Mutex
	shutdown   chan interface{}
	wait       sync.WaitGroup
	MaxItems   int   // Maximum number of files to cache
	MaxSize    int64 // Maximum file size to store
	ExpireItem int   // Seconds a file should be cached for
	Every      int   // Run an expiration check Every seconds，每隔多少秒检查一次缓存是否到期
}

// NewDefaultCache returns a new FileCache with sane defaults.
//初试化一个默认的缓存
func NewDefaultCache() *FileCache {
	return &FileCache{
		dur:        time.Since(time.Now()),
		items:      nil,
		in:         nil,
		MaxItems:   DefaultMaxItems,
		MaxSize:    DefaultMaxSize,
		ExpireItem: DefaultExpireItem,
		Every:      DefaultEvery,
	}
}

//缓存锁
func (cache *FileCache) lock() {
	cache.mutex.Lock()
}

//解锁缓存
func (cache *FileCache) unlock() {
	cache.mutex.Unlock()
}

//判断缓存是否为空
func (cache *FileCache) isCacheNull() bool {
	cache.lock()
	defer cache.unlock()
	return cache.items == nil
}

//获取缓存项
func (cache *FileCache) getItem(name string) (itm *cacheItem, ok bool) {
	if cache.isCacheNull() {
		return nil, false
	}
	cache.lock()
	defer cache.unlock()
	itm, ok = cache.items[name]
	return
}

// addItem is an internal function for adding an item to the cache.
//私有方法，添加一个项目到缓存中
func (cache *FileCache) addItem(name string) (err error) {
	if cache.isCacheNull() {
		return
	}
	ok := cache.InCache(name)
	expired := cache.itemExpired(name)
	if ok && !expired {
		return nil
	} else if ok {
		delete(cache.items, name)
	}

	itm, err := cacheFile(name, cache.MaxSize)
	if cache.items != nil && itm != nil {
		cache.lock()
		cache.items[name] = itm
		cache.unlock()
	} else {
		return
	}
	if !cache.InCache(name) {
		return ItemNotInCache
	}
	return nil
}

//删除一个缓存项
func (cache *FileCache) deleteItem(name string) {
	_, ok := cache.getItem(name)
	if ok {
		cache.lock()
		delete(cache.items, name)
		cache.unlock()
	}
}

// itemListener is a goroutine that listens for incoming files and caches
// them.
func (cache *FileCache) itemListener() {
	cache.wait.Add(1)
	for {
		select {
		case name := <-cache.in:
			cache.addItem(name)
		case <-cache.shutdown:
			cache.wait.Done()
			return
		}
	}
}

// expireOldest is used to expire the oldest item in the cache.
// The force argument is used to indicate it should remove at least one
// entry; for example, if a large number of files are cached at once, none
// may appear older than another.
//处理最老的一个过期项目；如果参数force为true，将会删除掉一个元素(删除最早过期的项目)，即使没有过期的项目。
func (cache *FileCache) expireOldest(force bool) {
	oldest := time.Now()
	oldestName := ""

	for name, itm := range cache.items {
		if force && oldestName == "" {
			oldest = itm.Lastaccess
			oldestName = name
		} else if itm.Lastaccess.Before(oldest) {
			oldest = itm.Lastaccess
			oldestName = name
		}
	}
	if oldestName != "" {
		cache.deleteItem(oldestName)
	}
}

// vacuum is a background goroutine responsible for cleaning the cache.
// It runs periodically, every cache.Every seconds. If cache.Every is set
// to 0, it will not run.
//一个后台goroutine，定期清除过期缓存。
func (cache *FileCache) vacuum() {
	if cache.Every < 1 {
		return
	}

	cache.wait.Add(1)
	for {
		select {
		case _ = <-cache.shutdown:
			cache.wait.Done()
			return
		case <-time.After(cache.dur):
			if cache.isCacheNull() {
				cache.wait.Done()
				return
			}
			for name, _ := range cache.items {
				if cache.itemExpired(name) {
					cache.deleteItem(name)
				}
			}
			for size := cache.Size(); size > cache.MaxItems; size = cache.Size() {
				cache.expireOldest(true)
			}
		}
	}
}

// FileChanged returns true if file should be expired based on mtime.
// If the file has changed on disk or no longer exists, it should be
// expired.
//判断文件是否被更新（硬盘更新或者被删除）
func (cache *FileCache) changed(name string) bool {
	itm, ok := cache.getItem(name)
	if !ok || itm == nil {
		return true
	}
	fi, err := os.Stat(name)
	if err != nil {
		return true
	} else if !itm.WasModified(fi) {
		return true
	}
	return false
}

// Expired returns true if the item has not been accessed recently.
//判断缓存项是否过期，如果一个项目从没访问过也会返回true
func (cache *FileCache) expired(name string) bool {
	itm, ok := cache.getItem(name)
	if !ok {
		return true
	}
	dur := itm.Dur()
	sec, err := strconv.Atoi(fmt.Sprintf("%0.0f", dur.Seconds()))
	if err != nil {
		return true
	} else if sec >= cache.ExpireItem {
		return true
	}
	return false
}

// itemExpired returns true if an item is expired.
//判断缓存项是否过期(被更新过也会视为过期)
func (cache *FileCache) itemExpired(name string) bool {
	if cache.changed(name) {
		return true
	} else if cache.ExpireItem != 0 && cache.expired(name) {
		return true
	}
	return false
}

// Active returns true if the cache has been started, and false otherwise.
//判断缓存是否开启
func (cache *FileCache) Active() bool {
	if cache.in == nil || cache.isCacheNull() {
		return false
	}
	return true
}

// Size returns the number of entries in the cache.
//返回缓存中的缓存项数目
func (cache *FileCache) Size() int {
	cache.lock()
	defer cache.unlock()
	return len(cache.items)
}

// FileSize returns the sum of the file sizes stored in the cache
//返回缓存中的所有文件大小的总和
func (cache *FileCache) FileSize() (totalSize int64) {
	cache.lock()
	defer cache.unlock()
	for _, itm := range cache.items {
		totalSize += itm.Size
	}
	return
}

// StoredFiles returns the list of files stored in the cache.
//返回缓存中的所有文件列表
func (cache *FileCache) StoredFiles() (fileList []string) {
	fileList = make([]string, 0, cache.Size())
	if cache.isCacheNull() || cap(fileList) == 0 {
		return
	}

	cache.lock()
	defer cache.unlock()
	for name, _ := range cache.items {
		fileList = append(fileList, name)
	}
	return
}

// InCache returns true if the item is in the cache.
//判断一个文件是否在缓存中
func (cache *FileCache) InCache(name string) bool {
	if cache.changed(name) {
		cache.deleteItem(name)
		return false
	}
	_, ok := cache.items[name]
	return ok
}

// WriteItem writes the cache item to the specified io.Writer.
//把缓存项写到io中去
func (cache *FileCache) WriteItem(w io.Writer, name string) (err error) {
	itm, ok := cache.getItem(name)
	if !ok {
		if !SquelchItemNotInCache {
			err = ItemNotInCache
		}
		return
	}
	r := itm.GetReader()
	itm.Lastaccess = time.Now()
	n, err := io.Copy(w, r)
	if err != nil {
		return
	} else if int64(n) != itm.Size {
		err = WriteIncomplete
		return
	}
	return
}

// GetItem returns the content of the item and a bool if name is present.
// GetItem should be used when you are certain an object is in the cache,
// or if you want to use the cache only.
func (cache *FileCache) GetItem(name string) (content []byte, ok bool) {
	itm, ok := cache.getItem(name)
	if !ok {
		return
	}
	content = itm.Access()
	return
}

// GetItemString is the same as GetItem, except returning a string.
func (cache *FileCache) GetItemString(name string) (content string, ok bool) {
	itm, ok := cache.getItem(name)
	if !ok {
		return
	}
	content = string(itm.Access())
	return
}

// ReadFile retrieves the file named by 'name'.
// If the file is not in the cache, load the file and cache the file in the
// background. If the file was not in the cache and the read was successful,
// the error ItemNotInCache is returned to indicate that the item was pulled
// from the filesystem and not the cache, unless the SquelchItemNotInCache
// global option is set; in that case, returns no error.
//根据文件名获取缓存中的文件内容
func (cache *FileCache) ReadFile(name string) (content []byte, err error) {
	if cache.InCache(name) {
		content, _ = cache.GetItem(name)
	} else {
		go cache.Cache(name)
		content, err = ioutil.ReadFile(name)
		if err == nil && !SquelchItemNotInCache {
			err = ItemNotInCache
		}
	}
	return
}

// ReadFileString is the same as ReadFile, except returning a string.
func (cache *FileCache) ReadFileString(name string) (content string, err error) {
	raw, err := cache.ReadFile(name)
	if err == nil {
		content = string(raw)
	}
	return
}

// WriteFile writes the file named by 'name' to the specified io.Writer.
// If the file is in the cache, it is loaded from the cache; otherwise,
// it is read from the filesystem and the file is cached in the background.
func (cache *FileCache) WriteFile(w io.Writer, name string) (err error) {
	if cache.InCache(name) {
		err = cache.WriteItem(w, name)
	} else {
		var fi os.FileInfo
		fi, err = os.Stat(name)
		if err != nil {
			return
		} else if fi.IsDir() {
			return ItemIsDirectory
		}
		go cache.Cache(name)
		var file *os.File
		file, err = os.Open(name)
		if err != nil {
			return
		}
		defer file.Close()
		_, err = io.Copy(w, file)

	}
	return
}

func (cache *FileCache) HttpWriteFile(w http.ResponseWriter, r *http.Request) {
	path, err := url.QueryUnescape(r.URL.String()) //对URL进行解码
	if err != nil {
		http.ServeFile(w, r, r.URL.Path)
	} else if len(path) > 1 {
		path = path[1:len(path)]
	} else {
		http.ServeFile(w, r, ".")
		return
	}

	//如果已经被缓存，则直接获取缓存的内容并返回。
	if cache.InCache(path) {
		itm := cache.items[path]
		ctype := http.DetectContentType(itm.Access())     //根据内容返回 MIME type
		mtype := mime.TypeByExtension(filepath.Ext(path)) //根据扩展名返回 MIME type
		if mtype != "" && mtype != ctype {
			ctype = mtype
		}
		header := w.Header()
		header.Set("content-length", fmt.Sprintf("%d", itm.Size))
		header.Set("content-disposition", fmt.Sprintf("filename=%s", filepath.Base(path)))
		header.Set("content-type", ctype)
		w.Write(itm.Access())
		return
	}
	go cache.Cache(path)       //缓存文件
	http.ServeFile(w, r, path) //返回文件的内容
}

// HttpHandler returns a valid HTTP handler for the given cache.
func HttpHandler(cache *FileCache) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		cache.HttpWriteFile(w, r)
	}
}

// Cache will store the file named by 'name' to the cache.
// This function doesn't return anything as it passes the file onto the
// incoming pipe; the file will be cached asynchronously. Errors will
// not be returned.
func (cache *FileCache) Cache(name string) {
	if cache.Size() == cache.MaxItems {
		cache.expireOldest(true)
	}
	cache.in <- name
}

// CacheNow immediately caches the file named by 'name'.
func (cache *FileCache) CacheNow(name string) (err error) {
	if cache.Size() == cache.MaxItems {
		cache.expireOldest(true)
	}
	return cache.addItem(name)
}

// Start activates the file cache; it will start up the background caching
// and automatic cache expiration goroutines and initialise the internal
// data structures.
//启动缓存运行。它将创建两个goroutines(C中叫子线程)，1负责在后台监听缓存请求，2负责自动删除过期的缓存。同事初始化结构体的数据。
func (cache *FileCache) Start() error {
	if cache.in != nil {
		close(cache.in)
		close(cache.shutdown)
	}
	dur, err := time.ParseDuration(fmt.Sprintf("%ds", cache.Every))
	if err != nil {
		return err
	}
	cache.dur = dur
	cache.items = make(map[string]*cacheItem, 0)
	cache.in = make(chan string, NewCachePipeSize)
	cache.shutdown = make(chan interface{}, 1)
	go cache.itemListener()
	go cache.vacuum()
	return nil
}

// Stop turns off the file cache.
// This closes the concurrent caching mechanism, destroys the cache, and
// the background scanner that it should stop.
// If there are any items or cache operations ongoing while Stop() is called,
// it is undefined how they will behave.
func (cache *FileCache) Stop() {
	if cache.in != nil {
		close(cache.in)
		close(cache.shutdown)
		<-time.After(1 * time.Microsecond) // give goroutines time to shutdown
	}

	if cache.items != nil {
		items := cache.StoredFiles()
		for _, name := range items {
			cache.deleteItem(name)
		}
		cache.lock()
		cache.items = nil
		cache.unlock()
	}
	cache.wait.Wait()
}

// RemoveItem immediately removes the item from the cache if it is present.
// It returns a boolean indicating whether anything was removed, and an error
// if an error has occurred.
func (cache *FileCache) Remove(name string) (ok bool, err error) {
	_, ok = cache.items[name]
	if !ok {
		return
	}
	cache.deleteItem(name)
	_, valid := cache.getItem(name)
	if valid {
		ok = false
	}
	return
}
