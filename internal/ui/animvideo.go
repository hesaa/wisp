package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GIFs. WhatsApp sends a GIF as a short silent MP4, so its frames come from Windows' Media
// Foundation source reader (the codecs Windows ships), converted to 32-bit pixels and read one at a
// time: nothing but the frame on show is kept. Slots and GUIDs are from mfreadwrite.h, mfobjects.h
// and mfapi.h.

var (
	mfplat                       = windows.NewLazySystemDLL("mfplat.dll")
	mfreadwrite                  = windows.NewLazySystemDLL("mfreadwrite.dll")
	ole32                        = windows.NewLazySystemDLL("ole32.dll")
	pMFStartup                   = mfplat.NewProc("MFStartup")
	pMFCreateAttributes          = mfplat.NewProc("MFCreateAttributes")
	pMFCreateMediaType           = mfplat.NewProc("MFCreateMediaType")
	pMFCreateSourceReaderFromURL = mfreadwrite.NewProc("MFCreateSourceReaderFromURL")
	pCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	pCoUninitialize              = ole32.NewProc("CoUninitialize")
	guidReaderAdvancedProcessing = windows.GUID{Data1: 0x0f81da2c, Data2: 0xb537, Data3: 0x4672, Data4: [8]byte{0xa8, 0xb2, 0xa6, 0x81, 0xb1, 0x73, 0x07, 0xa3}}
	guidMTMajorType              = windows.GUID{Data1: 0x48eba18e, Data2: 0xf8c9, Data3: 0x4687, Data4: [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	guidMTSubtype                = windows.GUID{Data1: 0xf7e34c9a, Data2: 0x42e8, Data3: 0x4714, Data4: [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	guidMTFrameSize              = windows.GUID{Data1: 0x1652c33d, Data2: 0xd6b2, Data3: 0x4012, Data4: [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	guidMTDefaultStride          = windows.GUID{Data1: 0x644b4e48, Data2: 0x1e02, Data3: 0x4516, Data4: [8]byte{0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6}}
	guidMediaTypeVideo           = windows.GUID{Data1: 0x73646976, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	iid2DBuffer                  = windows.GUID{Data1: 0x7dc9d5f9, Data2: 0x9ed9, Data3: 0x44ec, Data4: [8]byte{0x9b, 0xbf, 0x06, 0x00, 0xbb, 0x58, 0x9f, 0xbb}}
	guidVideoFormatRGB32         = windows.GUID{Data1: 22, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	mfStarted                    sync.Once
	mfStartErr                   error
	errNoFrame                   = errors.New("ui: the video gave no frame")
)

const (
	attrGetUINT32, attrGetUINT64, attrSetUINT32, attrSetGUID = 7, 8, 21, 24

	readerGetCurrentMediaType, readerSetCurrentMediaType, readerSetCurrentPosition, readerReadSample = 6, 7, 8, 9
	sampleGetDuration, sampleToContiguousBuffer                                                      = 37, 41
	bufferLock, bufferUnlock                                                                         = 3, 4
	buffer2DLock, buffer2DUnlock                                                                     = 3, 4 // IMF2DBuffer

	firstVideoStream = 0xfffffffc // MF_SOURCE_READER_FIRST_VIDEO_STREAM
	readerEndOfFile  = 0x2        // MF_SOURCE_READERF_ENDOFSTREAM
	maxVideoPixels   = 1 << 22
)

// videoFrames reads a video file's frames in a loop. Every call must come from the OS thread that
// made it (the caller locks its goroutine to one).
type videoFrames struct {
	reader       unsafe.Pointer
	w, h, stride int // stride < 0: the rows come bottom first
	bufs         [2][]byte
	flip         int
	uninit       bool
}

// newVideoFrames opens the video at path for frame-by-frame reading.
func newVideoFrames(path string) (*videoFrames, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	url, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}
	v := &videoFrames{}
	if hr, _, _ := pCoInitializeEx.Call(0, 0); int32(hr) >= 0 { // multithreaded; a failure: the thread has COM already
		v.uninit = true
	}
	mfStarted.Do(func() {
		if hr, _, _ := pMFStartup.Call(0x00020070, 1); int32(hr) < 0 { // MF_VERSION, MFSTARTUP_LITE
			mfStartErr = fmt.Errorf("ui: MFStartup: HRESULT %#x", uint32(hr))
		}
	})
	fail := func(what string, err error) (*videoFrames, error) {
		v.close()
		return nil, fmt.Errorf("ui: %s of %s: %w", what, filepath.Base(abs), err)
	}
	if mfStartErr != nil {
		return fail("Media Foundation", mfStartErr)
	}
	var attrs, mt, cur unsafe.Pointer
	if hr, _, _ := pMFCreateAttributes.Call(uintptr(unsafe.Pointer(&attrs)), 1); int32(hr) < 0 {
		return fail("attributes", fmt.Errorf("HRESULT %#x", uint32(hr)))
	}
	defer com(attrs, comRelease)
	// lets the reader convert the decoder's YUV frames to RGB32. The advanced processor, not the
	// basic one: that one hands over the decoder's padded picture (256x144 for a 250x140 video)
	// while calling it the smaller size.
	if err := com(attrs, attrSetUINT32, uintptr(unsafe.Pointer(&guidReaderAdvancedProcessing)), 1); err != nil {
		return fail("attributes", err)
	}
	if hr, _, _ := pMFCreateSourceReaderFromURL.Call(uintptr(unsafe.Pointer(url)), uintptr(attrs), uintptr(unsafe.Pointer(&v.reader))); int32(hr) < 0 {
		return fail("source reader", fmt.Errorf("HRESULT %#x", uint32(hr)))
	}
	if hr, _, _ := pMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mt))); int32(hr) < 0 {
		return fail("media type", fmt.Errorf("HRESULT %#x", uint32(hr)))
	}
	defer com(mt, comRelease)
	if err := errors.Join(
		com(mt, attrSetGUID, uintptr(unsafe.Pointer(&guidMTMajorType)), uintptr(unsafe.Pointer(&guidMediaTypeVideo))),
		com(mt, attrSetGUID, uintptr(unsafe.Pointer(&guidMTSubtype)), uintptr(unsafe.Pointer(&guidVideoFormatRGB32))),
		com(v.reader, readerSetCurrentMediaType, firstVideoStream, 0, uintptr(mt)),
		com(v.reader, readerGetCurrentMediaType, firstVideoStream, uintptr(unsafe.Pointer(&cur)))); err != nil {
		return fail("RGB output", err)
	}
	defer com(cur, comRelease)
	var size uint64
	if err := com(cur, attrGetUINT64, uintptr(unsafe.Pointer(&guidMTFrameSize)), uintptr(unsafe.Pointer(&size))); err != nil {
		return fail("frame size", err)
	}
	v.w, v.h = int(size>>32), int(uint32(size))
	if v.w <= 0 || v.h <= 0 || v.w*v.h > maxVideoPixels {
		return fail("frame size", fmt.Errorf("%dx%d", v.w, v.h))
	}
	var stride uint32
	v.stride = v.w * 4
	if com(cur, attrGetUINT32, uintptr(unsafe.Pointer(&guidMTDefaultStride)), uintptr(unsafe.Pointer(&stride))) == nil && int32(stride) != 0 {
		v.stride = int(int32(stride))
	}
	return v, nil
}

// next reads the next frame, starting over after the last one.
func (v *videoFrames) next() ([]byte, int, int, time.Duration, error) {
	for range 4 { // an end of file or a gap in the stream gives no sample: read again
		var flags uint32
		var when, length int64
		var sample, buf unsafe.Pointer
		if err := com(v.reader, readerReadSample, firstVideoStream, 0, 0, uintptr(unsafe.Pointer(&flags)),
			uintptr(unsafe.Pointer(&when)), uintptr(unsafe.Pointer(&sample))); err != nil {
			return nil, 0, 0, 0, err
		}
		if sample == nil {
			if flags&readerEndOfFile != 0 {
				start := propVariant{vt: vtI8}
				if err := com(v.reader, readerSetCurrentPosition, uintptr(unsafe.Pointer(&guidNull)), uintptr(unsafe.Pointer(&start))); err != nil {
					return nil, 0, 0, 0, err
				}
			}
			continue
		}
		pix, err := v.copyOut(sample, &buf)
		_ = com(sample, sampleGetDuration, uintptr(unsafe.Pointer(&length))) // 100 ns units; 0 when unknown
		com(sample, comRelease)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		show := time.Duration(length) * 100
		if show < minFrameDelay || show > time.Second {
			show = 66 * time.Millisecond
		}
		return pix, v.w, v.h, show, nil
	}
	return nil, 0, 0, 0, errNoFrame
}

// copyOut copies sample's picture, top row first, into the buffer not handed out last.
//
// The rows are read through the buffer's 2D interface, which gives the first row and the real
// distance between rows: a decoder pads rows of a width it does not like (not a multiple of 16), and
// the media type's default stride does not say so, so rows read at "width x 4" come out sheared.
func (v *videoFrames) copyOut(sample unsafe.Pointer, buf *unsafe.Pointer) ([]byte, error) {
	if err := com(sample, sampleToContiguousBuffer, uintptr(unsafe.Pointer(buf))); err != nil {
		return nil, err
	}
	defer com(*buf, comRelease)
	row := v.w * 4
	v.flip ^= 1
	if v.bufs[v.flip] == nil {
		v.bufs[v.flip] = make([]byte, row*v.h)
	}
	dst := v.bufs[v.flip]

	var two unsafe.Pointer
	if com(*buf, 0, uintptr(unsafe.Pointer(&iid2DBuffer)), uintptr(unsafe.Pointer(&two))) == nil && two != nil { // QueryInterface
		defer com(two, comRelease)
		var top unsafe.Pointer
		var pitch int32 // negative when the rows are stored bottom first: top is the top row either way
		if err := com(two, buffer2DLock, uintptr(unsafe.Pointer(&top)), uintptr(unsafe.Pointer(&pitch))); err != nil {
			return nil, err
		}
		defer com(two, buffer2DUnlock)
		if top == nil || int(max(pitch, -pitch)) < row {
			return nil, fmt.Errorf("ui: video rows %d bytes apart for a width of %d", pitch, v.w)
		}
		for y := 0; y < v.h; y++ {
			copy(dst[y*row:(y+1)*row], unsafe.Slice((*byte)(unsafe.Add(top, y*int(pitch))), row))
		}
		return dst, nil
	}

	// no 2D interface: one block of rows
	var data *byte
	var length uint32
	if err := com(*buf, bufferLock, uintptr(unsafe.Pointer(&data)), 0, uintptr(unsafe.Pointer(&length))); err != nil {
		return nil, err
	}
	defer com(*buf, bufferUnlock)
	step := rowStep(int(length), v.w, v.h)
	if data == nil || step < row {
		return nil, fmt.Errorf("ui: video frame of %d bytes for %dx%d", length, v.w, v.h)
	}
	src := unsafe.Slice(data, int(length))
	for y := 0; y < v.h; y++ {
		from := y
		if v.stride < 0 {
			from = v.h - 1 - y
		}
		copy(dst[y*row:(y+1)*row], src[from*step:from*step+row])
	}
	return dst, nil
}

// rowStep is the distance in bytes between the rows of a 32-bit picture w x h held in a block of
// length bytes: w x 4, unless the block is a padded picture (a decoder rounds sizes up, usually to
// 16), when it is the padded width that divides the block into at least h rows. 0: no such width.
func rowStep(length, w, h int) int {
	for padded := w; padded <= w+64; padded++ {
		if step := padded * 4; length%step == 0 && length/step >= h && length/step <= h+64 {
			return step
		}
	}
	return 0
}

func (v *videoFrames) close() {
	if v.reader != nil {
		com(v.reader, comRelease)
		v.reader = nil
	}
	if v.uninit {
		pCoUninitialize.Call()
		v.uninit = false
	}
}
