package transport

import "sync"

const streamReadBufferSize = 32 << 10

// Only scratch reads use this pool. RPC frames and retained SSE records keep
// their own copies so a later request cannot change an earlier response.
var streamReadBuffers = sync.Pool{
	New: func() any { return new([streamReadBufferSize]byte) },
}

func acquireStreamReadBuffer() *[streamReadBufferSize]byte {
	return streamReadBuffers.Get().(*[streamReadBufferSize]byte)
}

func releaseStreamReadBuffer(buffer *[streamReadBufferSize]byte) {
	// Avoid retaining one account's response bytes in the shared idle pool.
	clear(buffer[:])
	streamReadBuffers.Put(buffer)
}
