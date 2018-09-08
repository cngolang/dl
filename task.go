package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"strings"
)

type task struct {
	done          chan struct{}
	src           io.ReadCloser
	dst           io.WriteCloser
	bytePerSecond float64
	err           error
	startTime     time.Time
	endTime       time.Time
	mutex         sync.Mutex
	readNum       int64
	fileSize      int64
	filename      string
	buffer        []byte
	lim           *ratelimiter
	url           string
	isResume 	  bool
	header  map[string]string
}

func (t *task) getReadNum() int64 {
	if t == nil {
		return 0
	}
	return atomic.LoadInt64(&t.readNum)
}

func newTask(url string,h map[string]string) *task {
	lim, url := getLimitFromUrl(url)
	return &task{url: url, done: make(chan struct{}, 1), buffer: make([]byte, 32*1024), lim: &ratelimiter{lim: lim * 1000},header:h}
}

func (t *task) start() {
	defer func() {
		if err := recover(); err != nil {
			switch x := err.(type) {
			case string:
				t.err = errors.New(x)
			case error:
				t.err = x
			default:
				t.err = errors.New("Unknow panic")
			}
			close(t.done)
			t.endTime = time.Now()
		}
	}()
	var dst *os.File
	var rn, wn int
	var filename string
	var fi os.FileInfo
	req, _ := http.NewRequest("GET", t.url, nil)
	if t.header!=nil{
		for k,v:=range t.header{
			req.Header.Set(k,v)
		}
	}
	c := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
	rep, err := c.Do(req)
	if err != nil {
		goto done
	} else if rep.StatusCode != 200 &&rep.StatusCode != 206 {
		err = errors.New(fmt.Sprintf("wrong response %d", rep.StatusCode))
		goto done
	}

	filename, err = guessFilename(rep)

	fi, err = os.Stat(filename)

	if err == nil {
		if !fi.IsDir() {
			rep.Body.Close()
			if fi.Size()==rep.ContentLength{
				err=errors.New("File is downloaded! ")
				goto done
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", fi.Size()))
			rep, err = c.Do(req)
			if err != nil {
				goto done
			} else if rep.StatusCode != 200&&rep.StatusCode != 206 {
				err = errors.New(fmt.Sprintf("wrong response %d", rep.StatusCode))
				goto done
			}
			if rep.Header.Get("Accept-Ranges") == "bytes" ||rep.Header.Get("Content-Range") != ""{
				dst, err = os.OpenFile(filename, os.O_RDWR, 0666)
				if err != nil {
					goto done
				}
				dst.Seek(0, os.SEEK_END)
				t.readNum = fi.Size()
				t.isResume=true
			}
		}
	}

	if dst == nil {
		dst, err = os.Create(filename)
		if err != nil {
			goto done
		}
	}

	t.dst = dst
	t.src = rep.Body
	t.filename=filename
	if rep.ContentLength>0 &&t.isResume && fi!=nil{
		t.fileSize = rep.ContentLength+fi.Size()
	}else {
		t.fileSize = rep.ContentLength
	}


	go t.bps()

	t.startTime = time.Now()

loop:

	if t.lim.lim > 0 {
		t.lim.wait(t.readNum)
	}

	rn, err = t.src.Read(t.buffer)

	if rn > 0 {

		wn, err = t.dst.Write(t.buffer[:rn])

		if err != nil {
			goto done
		} else if rn != wn {
			err = io.ErrShortWrite
			goto done
		} else {
			atomic.AddInt64(&t.readNum, int64(rn))
			goto loop
		}
	}

done:
	t.err = err
	close(t.done)
	t.endTime = time.Now()
	return
}

func (t *task) bps() {
	var prev int64
	then := t.startTime

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return

		case now := <-ticker.C:
			d := now.Sub(then)
			then = now

			cur := t.getReadNum()
			bs := cur - prev
			prev = cur

			t.mutex.Lock()
			t.bytePerSecond = float64(bs) / d.Seconds()
			t.mutex.Unlock()
		}
	}
}

func (t *task) getSpeed() string {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return formatBytes(int64(t.bytePerSecond))
}

func (t *task) getETA() string {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.fileSize == 0 || t.bytePerSecond == 0 {
		return "      "
	} else {
		b:= formatTime((t.fileSize - t.getReadNum()) / int64(t.bytePerSecond))
		if len(b)>6{
			b=b[:6]
		}else if len(b)<6{
			b=strings.Join([]string{strings.Repeat(" ",6-len(b)),b},"")
		}
		return b
	}
}
