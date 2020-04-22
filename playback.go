package pulse

import "github.com/jfreymuth/pulse/proto"

// A PlaybackStream is used for playing audio.
// When creating a stream, the user must provide a callback that will be used to buffer audio data.
type PlaybackStream struct {
	c         *Client
	index     uint32
	running   bool
	ended     bool
	underflow bool
	err       error

	buf            []byte
	bytesPerSample int

	cb func([]byte) error

	createRequest proto.CreatePlaybackStream
	createReply   proto.CreatePlaybackStreamReply
}

// EndOfData is a special error value that can be returned by a callback to stop the stream.
const EndOfData endOfData = false

// NewPlayback creates a playback stream.
// The type of cb must be one of the following:
//   - func([]byte)
//   - func([]int16)
//   - func([]int32)
//   - func([]float32)
//   - func([]byte) error
//   - func([]int16) error
//   - func([]int32) error
//   - func([]float32) error
// If the callback returns any error, the stream will be stopped. The special error value EndOfData
// can be used to intentionally stop the stream from within the callback.
//
// The created stream wil not be running, it must be started with Start().
// The order of options is important in some cases, see the documentation of the individual PlaybackOptions.
func (c *Client) NewPlayback(cb interface{}, opts ...PlaybackOption) (*PlaybackStream, error) {
	p := &PlaybackStream{
		c: c,
		createRequest: proto.CreatePlaybackStream{
			SinkIndex:             proto.Undefined,
			ChannelMap:            proto.ChannelMap{proto.ChannelMono},
			SampleSpec:            proto.SampleSpec{Channels: 1, Rate: 44100},
			BufferMaxLength:       proto.Undefined,
			Corked:                true,
			BufferTargetLength:    proto.Undefined,
			BufferPrebufferLength: proto.Undefined,
			BufferMinimumRequest:  proto.Undefined,
			Properties:            map[string]string{},
		},
		ended: true,
	}

	switch cb := cb.(type) {
	case func([]byte) error:
		p.cb = cb
		p.createRequest.Format = proto.FormatUint8
		p.bytesPerSample = 1
	case func([]byte):
		p.cb = func(buf []byte) error { cb(buf); return nil }
		p.createRequest.Format = proto.FormatUint8
		p.bytesPerSample = 1
	case func([]int16) error:
		p.cb = func(buf []byte) error { return cb(int16Slice(buf)) }
		p.createRequest.Format = formatI16
		p.bytesPerSample = 2
	case func([]int16):
		p.cb = func(buf []byte) error { cb(int16Slice(buf)); return nil }
		p.createRequest.Format = formatI16
		p.bytesPerSample = 2
	case func([]int32) error:
		p.cb = func(buf []byte) error { return cb(int32Slice(buf)) }
		p.createRequest.Format = formatI32
		p.bytesPerSample = 4
	case func([]int32):
		p.cb = func(buf []byte) error { cb(int32Slice(buf)); return nil }
		p.createRequest.Format = formatI32
		p.bytesPerSample = 4
	case func([]float32) error:
		p.cb = func(buf []byte) error { return cb(float32Slice(buf)) }
		p.createRequest.Format = formatF32
		p.bytesPerSample = 4
	case func([]float32):
		p.cb = func(buf []byte) error { cb(float32Slice(buf)); return nil }
		p.createRequest.Format = formatF32
		p.bytesPerSample = 4
	default:
		panic("pulse: invalid callback type")
	}

	for _, opt := range opts {
		opt(p)
	}

	if p.createRequest.ChannelVolumes == nil {
		cvol := make(proto.ChannelVolumes, len(p.createRequest.ChannelMap))
		for i := range cvol {
			cvol[i] = 0x100
		}
		p.createRequest.ChannelVolumes = cvol
	}

	err := c.c.Request(&p.createRequest, &p.createReply)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.playback[p.index] = p
	c.mu.Unlock()
	return p, nil
}

func (p *PlaybackStream) buffer(n int) []byte {
	if n > len(p.buf) {
		p.buf = make([]byte, n)
	}
	err := p.cb(p.buf[:n])
	if err != nil {
		if err != EndOfData {
			p.err = err
		}
		p.ended = true
		return nil
	}
	return p.buf[:n]
}

// Start starts playing audio.
func (p *PlaybackStream) Start() {
	p.c.c.Request(&proto.FlushPlaybackStream{StreamIndex: p.index}, nil)
	p.ended = false
	buf := p.buffer(int(p.createReply.BufferTargetLength))
	if buf == nil {
		return
	}
	p.c.c.Send(p.index, buf)
	p.c.c.Request(&proto.CorkPlaybackStream{StreamIndex: p.index, Corked: false}, nil)
	p.running = true
	p.underflow = false
}

// Stop stops playing audio; the callback will no longer be called.
// If the buffer size/latency is large, audio may continue to play for some time after the call to Stop.
func (p *PlaybackStream) Stop() {
	p.ended = true
}

// Pause stops playing audio immediately.
func (p *PlaybackStream) Pause() {
	p.c.c.Request(&proto.CorkPlaybackStream{StreamIndex: p.index, Corked: true}, nil)
	p.running = false
}

// Resume resumes a paused stream.
func (p *PlaybackStream) Resume() {
	if !p.ended {
		p.c.c.Request(&proto.CorkPlaybackStream{StreamIndex: p.index, Corked: false}, nil)
		p.running = true
		p.underflow = false
	}
}

// Drain waits until the playback has ended.
// Drain does not return when the stream is paused.
// If the stream's callback never returns an error, Drain may block forever.
func (p *PlaybackStream) Drain() {
	if p.running {
		p.c.c.Request(&proto.DrainPlaybackStream{StreamIndex: p.index}, nil)
	}
}

// Close closes the stream.
// Calling methods on a closed stream may panic.
func (p *PlaybackStream) Close() {
	p.c.c.Request(&proto.DeletePlaybackStream{StreamIndex: p.index}, nil)
	p.running = false
	p.c = nil
}

// Closed returns wether the stream was closed.
// Calling other methods on a closed stream may panic.
func (p *PlaybackStream) Closed() bool {
	return p.c == nil
}

// Running returns wether the stream is currently playing.
func (p *PlaybackStream) Running() bool { return p.running }

// Underflow returns true if any underflows happend since the last call to Start or Resume.
// Underflows usually happen because the latency/buffer size is too low or because the callback
// takes too long to run.
func (p *PlaybackStream) Underflow() bool { return p.underflow }

// Error returns the last error returned by the stream's callback.
func (p *PlaybackStream) Error() error { return p.err }

// SampleRate returns the stream's sample rate (samples per second).
func (p *PlaybackStream) SampleRate() int {
	return int(p.createReply.Rate)
}

// Channels returns the number of channels.
func (p *PlaybackStream) Channels() int {
	return int(p.createReply.Channels)
}

// BufferSize returns the size of the server-side buffer in samples.
func (p *PlaybackStream) BufferSize() int {
	s := int(p.createReply.BufferTargetLength) / int(p.createReply.Channels)
	return s / p.bytesPerSample
}

// BufferSizeBytes returns the size of the server-side buffer in bytes.
func (p *PlaybackStream) BufferSizeBytes() int {
	return int(p.createReply.BufferTargetLength)
}

// StreamIndex returns the stream index.
// This should only be used together with (*Cient).RawRequest.
func (p *PlaybackStream) StreamIndex() uint32 {
	return p.index
}

// A PlaybackOption supplies configuration when creating streams.
type PlaybackOption func(*PlaybackStream)

// PlaybackMono sets a stream to a single channel.
var PlaybackMono PlaybackOption = func(p *PlaybackStream) {
	p.createRequest.ChannelMap = proto.ChannelMap{proto.ChannelMono}
	p.createRequest.Channels = 1
}

// PlaybackStereo sets a stream to two channels.
var PlaybackStereo PlaybackOption = func(p *PlaybackStream) {
	p.createRequest.ChannelMap = proto.ChannelMap{proto.ChannelLeft, proto.ChannelRight}
	p.createRequest.Channels = 2
}

// PlaybackChannels sets a stream to use a custom channel map.
func PlaybackChannels(m proto.ChannelMap) PlaybackOption {
	if len(m) == 0 {
		panic("pulse: invalid channel map")
	}
	return func(p *PlaybackStream) {
		p.createRequest.ChannelMap = m
		p.createRequest.Channels = byte(len(m))
	}
}

// PlaybackSampleRate sets the stream's sample rate.
func PlaybackSampleRate(rate int) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.Rate = uint32(rate)
	}
}

// PlaybackBufferSize sets the size of the server-side buffer.
// Setting the buffer size too small causes underflows, resulting in audible artifacts.
//
// Buffer size and latency should not be set at the same time.
func PlaybackBufferSize(samples int) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.BufferTargetLength = uint32(samples * p.bytesPerSample)
		p.createRequest.AdjustLatency = false
	}
}

// PlaybackLatency sets the stream's latency in seconds.
// Setting the latency too low causes underflows, resulting in audible artifacts.
// Applications should generally use the highest acceptable latency.
//
// This should be set after sample rate and channel options.
//
// Buffer size and latency should not be set at the same time.
func PlaybackLatency(seconds float64) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.BufferTargetLength = uint32(seconds*float64(p.createRequest.Rate)) * uint32(p.createRequest.Channels) * uint32(p.bytesPerSample)
		p.createRequest.BufferMaxLength = 2 * p.createRequest.BufferTargetLength
		p.createRequest.AdjustLatency = true
	}
}

// PlaybackSink sets the sink the stream should send audio to.
func PlaybackSink(sink *Sink) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.SinkIndex = sink.info.SinkIndex
	}
}

// PlaybackLowLatency sets the latency to the lowest safe value, as recommended by the pulseaudio server.
//
// This should be set after sample rate and channel options.
//
// Buffer size and latency should not be set at the same time.
func PlaybackLowLatency(sink *Sink) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.SinkIndex = sink.info.SinkIndex
		p.createRequest.BufferTargetLength = uint32(uint64(sink.info.RequestedLatency)*uint64(p.createRequest.Rate)/1000000) * uint32(p.createRequest.Channels) * uint32(p.bytesPerSample)
		p.createRequest.BufferMaxLength = 2 * p.createRequest.BufferTargetLength
		p.createRequest.AdjustLatency = true
	}
}

// PlaybackMediaName sets the streams media name.
// This will e.g. be displayed by a volume control application to identity the stream.
func PlaybackMediaName(name string) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.Properties["media.name"] = name
	}
}

// PlaybackMediaIconName sets the streams media icon using an xdg icon name.
// This will e.g. be displayed by a volume control application to identity the stream.
func PlaybackMediaIconName(name string) PlaybackOption {
	return func(p *PlaybackStream) {
		p.createRequest.Properties["media.icon_name"] = name
	}
}

// PlaybackRawOption can be used to create custom options.
//
// This is an advanced function, similar to (*Client).RawRequest.
func PlaybackRawOption(o func(*proto.CreatePlaybackStream)) PlaybackOption {
	return func(p *PlaybackStream) {
		o(&p.createRequest)
	}
}

type endOfData bool

func (endOfData) Error() string { return "end of data" }
