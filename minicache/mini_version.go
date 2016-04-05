/*
   ijibu 写了一个文件缓存最小化的版本。
	思考如何提高并发
*/
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	InvalidCacheItem = errors.New("invalid cache item")
	ItemIsDirectory  = errors.New("can't cache a directory")
	ItemNotInCache   = errors.New("item not in cache")
	ItemTooLarge     = errors.New("item too large for cache")
	WriteIncomplete  = errors.New("incomplete write of cache item")
)

var (
	cache *FileCache
)

//存在并发访问的情况下，就需要加锁了。
type cacheItem struct {
	content    []byte     //缓存的内容
	Modified   time.Time  //最近修改时间
	Lastaccess time.Time  //最后访问时间
	Size       int64      //缓存大小
	lock       sync.Mutex //锁
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

var handler func(http.ResponseWriter, *http.Request)

type FileCache struct {
	wait       sync.WaitGroup
	in         chan string
	shutdown   chan interface{}
	items      map[string]*cacheItem
	MaxSize    int64
	dur        time.Duration
	MaxItems   int //最多缓存的数量
	mutex      sync.Mutex
	ExpireItem int // Seconds a file should be cached for
	Every      int // Run an expiration check Every seconds，每隔多少秒检查一次缓存是否到期
}

//缓存锁
func (cache *FileCache) lock() {
	cache.mutex.Lock()
}

//解锁缓存
func (cache *FileCache) unlock() {
	cache.mutex.Unlock()
}

// HttpHandler returns a valid HTTP handler for the given cache.
func HttpHandler(cache *FileCache) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		cache.HttpWriteFile(w, r)
	}
}

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
	//适当把in调大一些，可以提高并发访问缓存的能力。
	cache.in = make(chan string, 400)
	cache.shutdown = make(chan interface{}, 1)
	cache.items = make(map[string]*cacheItem, 0)
	cache.MaxSize = 1024 * 1024 * 16
	cache.Every = 60
	cache.MaxItems = 60
	go cache.itemListener()
	go cache.vacuum()
	return nil
}

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

//一个后台goroutine，定期清除过期缓存。
func (cache *FileCache) vacuum() {
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

//返回缓存中的缓存项数目
func (cache *FileCache) Size() int {
	cache.lock()
	defer cache.unlock()
	return len(cache.items)
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

//判断文件是否被更新（硬盘更新或者被删除）
func (cache *FileCache) changed(name string) bool {
	itm, ok := cache.getItem(name)
	if !ok || itm == nil {
		return true
	}
	fi, err := os.Stat("xml/" + name)
	if err != nil {
		return true
	} else if !itm.WasModified(fi) {
		return true
	}
	return false
}

//判断缓存项是否过期(被更新过也会视为过期)
func (cache *FileCache) itemExpired(name string) bool {
	if cache.changed(name) {
		return true
	} else if cache.ExpireItem != 0 && cache.expired(name) {
		return true
	}
	return false
}

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

//判断缓存是否为空
func (cache *FileCache) isCacheNull() bool {
	cache.lock()
	defer cache.unlock()
	return cache.items == nil
}

func (cache *FileCache) addItem(name string) (err error) {
	itm, err := cacheFile(name, cache.MaxSize)
	if cache.items != nil && itm != nil {
		cache.items[name] = itm
	} else {
		return
	}

	return nil
}

func (cache *FileCache) Cache(name string) {
	cache.in <- name
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
	r.ParseForm() //解析参数，默认是不会解析的
	//	fmt.Println(r.Form) //这些信息是输出到服务器端的打印信息
	//	fmt.Println("path", r.URL.Path)
	//	fmt.Println("scheme", r.URL.Scheme)
	//	fmt.Println(r.Form["url_long"])
	//
	//	for k, v := range r.Form {
	//		fmt.Println("key:", k)
	//		fmt.Println("val:", strings.Join(v, ""))
	//	}

	name := r.Form["name"]
	fileName := strings.Join(name, "")
	if len(fileName) == 0 {
		return
	}
	if cache.InCache(fileName) {
		log.Printf("read %s from cache\n", fileName)
		itm := cache.items[fileName]
		ctype := http.DetectContentType(itm.Access())                  //根据内容返回 MIME type
		mtype := mime.TypeByExtension(filepath.Ext("xml/" + fileName)) //根据扩展名返回 MIME type
		if mtype != "" && mtype != ctype {
			ctype = mtype
		}
		header := w.Header()
		header.Set("content-length", fmt.Sprintf("%d", itm.Size))
		header.Set("content-disposition", fmt.Sprintf("filename=%s", filepath.Base("xml/"+fileName)))
		header.Set("content-type", ctype)
		w.Write(itm.Access())
		return
	}

	log.Printf("read %s from desk\n", fileName)
	go cache.Cache(fileName)              //缓存文件
	http.ServeFile(w, r, "xml/"+fileName) //返回文件的内容
}

//判断一个文件是否在缓存中
func (cache *FileCache) InCache(name string) bool {
	_, ok := cache.items[name]
	return ok
}

//缓存文件
func cacheFile(path string, maxSize int64) (itm *cacheItem, err error) {
	fi, err := os.Stat("xml/" + path)
	if err != nil {
		return
	} else if fi.Mode().IsDir() {
		return nil, ItemIsDirectory
	} else if fi.Size() > maxSize {
		return nil, ItemTooLarge
	}

	content, err := ioutil.ReadFile("xml/" + path)
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

func (cache *FileCache) HttpRemove(w http.ResponseWriter, r *http.Request) {
	r.ParseForm() //解析参数，默认是不会解析的
	name := r.Form["name"]
	fileName := strings.Join(name, "")

	if cache.InCache(fileName) {
		log.Printf("delete %s from cache\n", fileName)
		cache.Remove(fileName)
		return
	}

	return
}

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

func main() {
	cache := &FileCache{}
	//handler = HttpHandler(cache)
	cache.Start()
	http.HandleFunc("/", cache.HttpWriteFile)
	//http.HandleFunc("/", Dispatch)
	http.HandleFunc("/delete", cache.HttpRemove)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
