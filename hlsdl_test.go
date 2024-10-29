package hlsdl

import (
	"fmt"
	"os"
	"testing"
)

func TestDescrypt(t *testing.T) {
	segs, err := parseHlsSegments("https://cdn.theoplayer.com/video/big_buck_bunny_encrypted/stream-800/index.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}

	hlsDl := New("https://cdn.theoplayer.com/video/big_buck_bunny_encrypted/stream-800/index.m3u8", nil, "./download", "", "", 2, false)
	seg := segs[0]
	seg.Path = fmt.Sprintf("%s/seg%d.ts", hlsDl.dir, seg.SeqId)
	if err := hlsDl.downloadSegment(seg); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(seg.Path)

	if _, err := hlsDl.decrypt(seg, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
}

func TestDownload(t *testing.T) {
	hlsDl := New("https://h5dfsg.anwangjd1.com/api/app/media/h5/m3u8/sp/ad/ef/q2/lo/e1b620caf03f43f7b3e43209911d4d56.m3u8?token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJwdWJsaWMiLCJleHAiOjE3MzI3NzQyODYsImlzc3VlciI6ImNvbS5idXR0ZXJmbHkiLCJzdWIiOiJhc2lnbiIsInVzZXJJZCI6OTU0NDE1OX0.5IM6AZTASHnvFn8-Jk9lz3b48ZuAUsO6ZHiVherLqrI", nil, "./download", "", "", 2, false)
	filepath, err := hlsDl.Download()
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath)
}
