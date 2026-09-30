package demux

import "strconv"

// MatroskaTrack describes an audio track that is rewrapped into an audio-only Matroska
// stream for ffmpeg. That is how codecs without their own framing (AAC in MP4/MKV, Opus,
// Vorbis, FLAC) reach the decoder, so a minimal ffmpeg needs only the Matroska demuxer.
type MatroskaTrack struct {
	CodecID      string // Matroska codec ID, e.g. A_AAC, A_OPUS
	CodecPrivate []byte
	SampleRate   float64
	Channels     int
	BitDepth     int // PCM only
}

// AudioFormat tells the decoder how to hand the frames of one audio stream to ffmpeg:
// either raw (self-framed codecs such as MP3, AC-3, DTS, ADTS AAC) or rewrapped into
// Matroska.
type AudioFormat struct {
	Args     []string       // ffmpeg input options for a raw stream, e.g. -f ac3
	Matroska *MatroskaTrack // when set, frames are rewrapped and Args is ignored
}

// Valid reports whether f says how to decode anything.
func (f AudioFormat) Valid() bool { return f.Matroska != nil || len(f.Args) > 0 }

// InputArgs are the ffmpeg input options that read frames in this format from stdin.
func (f AudioFormat) InputArgs() []string {
	if f.Matroska != nil {
		return []string{"-f", "matroska"}
	}
	return f.Args
}

// Self-framed codecs that ffmpeg reads raw, by the name of ffmpeg's demuxer for them.
// Each container maps its own tags to these; rawFormat has the ffmpeg arguments.
const (
	codecMP3  = "mp3"  // MPEG-1/2 layers I-III
	codecAC3  = "ac3"  // AC-3
	codecEAC3 = "eac3" // E-AC-3
	codecDTS  = "dts"  // DTS (core)
	codecADTS = "aac"  // AAC with ADTS headers
	codecLATM = "loas" // AAC in LATM/LOAS
)

// rawFormat is how ffmpeg reads a self-framed codec (codec*) raw; the zero AudioFormat for
// any other name.
func rawFormat(codec string) AudioFormat {
	switch codec {
	case codecAC3, codecEAC3:
		// downmixing inside the decoder is ~25% cheaper than decoding 5.1 and downmixing after
		return AudioFormat{Args: []string{"-downmix", "mono", "-f", codec}}
	case codecDTS:
		return AudioFormat{Args: []string{"-core_only", "1", "-f", codec}} // skip lossless/XBR extensions
	case codecMP3, codecADTS, codecLATM:
		return AudioFormat{Args: []string{"-f", codec}}
	}
	return AudioFormat{}
}

// pcmFormat is how ffmpeg reads raw PCM of sample format f (ffmpeg's name: s16le, u8, …).
func pcmFormat(f string, rate, channels int) AudioFormat {
	return AudioFormat{Args: []string{"-f", f, "-ar", strconv.Itoa(rate), "-ac", strconv.Itoa(channels)}}
}
