package hlsdl

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/grafov/m3u8"
	"gopkg.in/cheggaaa/pb.v1"
)

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

// HlsDl present a HLS downloader
type HlsDl struct {
	client     *resty.Client
	headers    map[string]string
	dir        string
	hlsURL     string
	workers    int
	bar        *pb.ProgressBar
	enableBar  bool
	filename   string
	startTime  int64
	segTotal   int64
	segCurrent int64
	proxy      string
}

type Segment struct {
	*m3u8.MediaSegment
	Path string
}

type DownloadResult struct {
	Err   error
	SeqId uint64
}

func New(hlsURL string, headers map[string]string, dir, filename, proxy string, workers int, enableBar bool) *HlsDl {
	if filename == "" {
		filename = getFilename()
	}
	client := resty.New().SetTimeout(30 * time.Minute).SetRetryCount(3)

	if proxy != "" {
		client.SetProxy(proxy)
	}

	hlsdl := &HlsDl{
		hlsURL:    hlsURL,
		dir:       dir,
		client:    client,
		workers:   workers,
		enableBar: enableBar,
		headers:   headers,
		filename:  filename,
		startTime: time.Now().UnixMilli(),
		proxy:     proxy,
	}

	return hlsdl
}

func wait(wg *sync.WaitGroup) chan bool {
	c := make(chan bool, 1)
	go func() {
		wg.Wait()
		c <- true
	}()
	return c
}

func silently(_ ...interface{}) {}

func closeq(v interface{}) {
	if c, ok := v.(io.Closer); ok {
		silently(c.Close())
	}
}

func (hlsDl *HlsDl) downloadSegment(segment *Segment) error {
	for i := 0; i < 30; i++ {
		hlsDl.client.SetRetryCount(5).SetRetryWaitTime(time.Second * 2)
		resp, err := hlsDl.client.R().SetHeaders(hlsDl.headers).SetDoNotParseResponse(true).Get(segment.URI)
		if err != nil {
			return err
		}
		defer resp.RawBody().Close()
		data, err := io.ReadAll(resp.RawBody())
		if err == nil {
			if resp.StatusCode() != http.StatusOK {
				continue
			}

			// create local file
			outFile, err := os.Create(segment.Path)
			if err != nil {
				return err
			}
			defer closeq(outFile)

			// write file
			_, err = outFile.Write(data)
			if err != nil {
				return err
			}
			return nil
		}
		if errors.Is(err, io.EOF) {
			log.Printf("Download segment %d failed, %d retrying\n", segment.SeqId, i+1)
			continue
		}
		if err.Error() == "unexpected EOF" {
			log.Printf("Download segment %d failed, %d retrying\n", segment.SeqId, i+1)
			continue
		}

		return err
	}
	return nil
}

func (hlsDl *HlsDl) downloadSegments(segmentsDir string, segments []*Segment) error {
	wg := &sync.WaitGroup{}
	wg.Add(hlsDl.workers)
	finishedChan := wait(wg)
	quitChan := make(chan bool)
	segmentChan := make(chan *Segment)
	downloadResultChan := make(chan *DownloadResult, hlsDl.workers)
	for i := 0; i < hlsDl.workers; i++ {
		go func() {
			defer wg.Done()
			for segment := range segmentChan {
				tried := 0
			DOWNLOAD:
				tried++
				select {
				case <-quitChan:
					return
				default:
				}
				if err := hlsDl.downloadSegment(segment); err != nil {
					if strings.Contains(err.Error(), "connection reset by peer") && tried < 3 {
						time.Sleep(time.Second)
						log.Println("Retry download segment ", segment.SeqId)
						goto DOWNLOAD
					}
					downloadResultChan <- &DownloadResult{Err: err, SeqId: segment.SeqId}
					return
				}
				downloadResultChan <- &DownloadResult{SeqId: segment.SeqId}
			}
		}()
	}

	go func() {
		defer close(segmentChan)
		for _, segment := range segments {
			segName := fmt.Sprintf("Seg%d.ts", segment.SeqId)
			segment.Path = filepath.Join(segmentsDir, segName)
			select {
			case segmentChan <- segment:
			case <-quitChan:
				return
			}
		}
	}()
	hlsDl.segTotal = int64(len(segments))
	if hlsDl.enableBar {
		hlsDl.bar = pb.New(len(segments)).SetMaxWidth(100).Prefix("Downloading...")
		hlsDl.bar.ShowElapsedTime = true
		hlsDl.bar.Start()
	}

	defer func() {
		if hlsDl.enableBar {
			hlsDl.bar.Finish()
		}
	}()

	for {
		select {
		case <-finishedChan:
			return nil
		case result := <-downloadResultChan:
			if result.Err != nil {
				close(quitChan)
				return result.Err
			}
			if hlsDl.enableBar {
				hlsDl.bar.Increment()
			} else {
				atomic.AddInt64(&hlsDl.segCurrent, 1)
			}
		}
	}

}

func (hlsDl *HlsDl) join(segmentsDir string, segments []*Segment) (string, error) {
	log.Println("Joining segments")

	outFile := filepath.Join(hlsDl.dir, hlsDl.filename)

	f, err := os.Create(outFile)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sort.Slice(segments, func(i, j int) bool {
		return segments[i].SeqId < segments[j].SeqId
	})

	// 仅仅获取一次Key即可
	key, iv, err := hlsDl.getKey(segments[0])
	if err != nil {
		return "", err
	}

	defer os.RemoveAll(segmentsDir)
	for _, segment := range segments {
		var d []byte
		var err error
		d, err = hlsDl.decrypt(segment, key, iv)
		if err.Error() == "crypto/cipher: input not full blocks" {
			return "", err
		}

		if err != nil {
			d, err = hlsDl.decryptWithKey(segment)
		}
		if _, err := f.Write(d); err != nil {
			return "", err
		}
		if err := os.RemoveAll(segment.Path); err != nil {
			return "", err
		}
	}

	return outFile, nil
}

func (hlsDl *HlsDl) Download() (string, error) {
	segs, err := parseHlsSegments(hlsDl.hlsURL, hlsDl.headers)
	if err != nil {
		return "", err
	}
	segmentsDir := filepath.Join(hlsDl.dir, fmt.Sprintf("%d", hlsDl.startTime))
	if err := os.MkdirAll(segmentsDir, os.ModePerm); err != nil {
		return "", err
	}
	if err := hlsDl.downloadSegments(segmentsDir, segs); err != nil {
		return "", err
	}
	fp, err := hlsDl.join(segmentsDir, segs)
	if err != nil {
		return "", err
	}

	return fp, nil
}

// Decrypt descryps a segment
func (hlsDl *HlsDl) decrypt(segment *Segment, key, iv []byte) ([]byte, error) {
	// 捕获潜在的 panic 并将其转化为错误
	var err error
	var data []byte // 提前声明 data 为 nil，以便在发生 panic 时可以安全返回

	defer func() {
		if r := recover(); r != nil {
			// 将 panic 的内容转化为字符串，检查是否包含目标信息
			if panicErr, ok := r.(string); ok && panicErr == "crypto/cipher: input not full blocks" {
				err = fmt.Errorf(panicErr)
			} else {
				err = fmt.Errorf("unexpected panic during AES decryption: %v", r)
			}
		}
	}()

	file, err := os.Open(segment.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err = io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	if segment.Key != nil {
		// 调用可能会导致 panic 的解密函数
		data, err = decryptAES128(data, key, iv)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt AES128 segment data: %w", err)
		}
	}

	for j := 0; j < len(data); j++ {
		if data[j] == syncByte {
			data = data[j:]
			break
		}
	}

	return data, err
}

// Decrypt descryps a segment
func (hlsDl *HlsDl) decryptWithKey(segment *Segment) ([]byte, error) {
	file, err := os.Open(segment.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if segment.Key != nil {
		key, iv, err := hlsDl.getKey(segment)
		if err != nil {
			return nil, err
		}
		data, err = decryptAES128(data, key, iv)
		if err != nil {
			return nil, err
		}
	}

	for j := 0; j < len(data); j++ {
		if data[j] == syncByte {
			data = data[j:]
			break
		}
	}

	return data, nil
}

func (hlsDl *HlsDl) getKey(segment *Segment) (key []byte, iv []byte, err error) {
	for i := 0; i < 10; i++ {
		res, err := hlsDl.client.SetHeaders(hlsDl.headers).R().Get(segment.Key.URI)
		if err != nil {
			continue
		}
		if res.StatusCode() != 200 {
			continue
			return nil, nil, errors.New("Failed to get descryption key")
		}
		key = res.Body()
		iv = []byte(segment.Key.IV)
		if len(iv) == 0 {
			iv = defaultIV(segment.SeqId)
		}
		return key, iv, nil
	}
	return nil, nil, errors.New("Failed to get descryption key")
}

func (hlsDl *HlsDl) GetProgress() float64 {
	var current int64
	if hlsDl.enableBar {
		current = hlsDl.bar.Get()
	} else {
		current = atomic.LoadInt64(&hlsDl.segCurrent)
	}
	return float64(current) / float64(hlsDl.segTotal)
}
