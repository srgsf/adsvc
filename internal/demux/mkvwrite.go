package demux

// The audio-only Matroska writer: frames of a MatroskaTrack are rewrapped into this stream
// for `ffmpeg -f matroska -i pipe:0` (internal/decode).

// AppendHeader appends the start of an audio-only Matroska stream with t as its one track,
// numbered 1, and millisecond timestamps. The segment has an unknown size so it can be streamed.
func (t *MatroskaTrack) AppendHeader(b []byte) []byte {
	var head []byte
	head = appendUint(head, elEBMLVersion, 1)
	head = appendUint(head, elEBMLReadVer, 1)
	head = appendUint(head, elEBMLMaxIDLen, 4)
	head = appendUint(head, elEBMLMaxSzLen, 8)
	head = appendElement(head, elDocType, []byte("matroska"))
	head = appendUint(head, elDocTypeVer, 4)
	head = appendUint(head, elDocTypeReadV, 2)
	out := appendElement(b, elHeader, head)

	out = appendID(out, elSegment)
	out = append(out, ebmlUnknownSize...)

	var info []byte
	info = appendUint(info, elTimecodeScale, 1_000_000)
	info = appendElement(info, elMuxingApp, []byte("adsvc"))
	info = appendElement(info, elWritingApp, []byte("adsvc"))
	out = appendElement(out, elInfo, info)

	var audio []byte
	if t.SampleRate > 0 {
		audio = appendFloat(audio, elSamplingFreq, t.SampleRate)
	}
	if t.Channels > 0 {
		audio = appendUint(audio, elChannels, uint64(t.Channels))
	}
	if t.BitDepth > 0 {
		audio = appendUint(audio, elBitDepth, uint64(t.BitDepth))
	}
	var entry []byte
	entry = appendUint(entry, elTrackNumber, 1)
	entry = appendUint(entry, elTrackUID, 1)
	entry = appendUint(entry, elTrackType, mkvTrackAudio)
	entry = appendElement(entry, elCodecID, []byte(t.CodecID))
	if len(t.CodecPrivate) > 0 {
		entry = appendElement(entry, elCodecPrivate, t.CodecPrivate)
	}
	entry = appendElement(entry, elAudio, audio)
	return appendElement(out, elTracks, appendElement(nil, elTrackEntry, entry))
}

// AppendMatroskaFrame appends one frame of the track at ms milliseconds, as its own cluster:
// cluster timestamps are absolute, so frames never need a relative timestamp that could
// overflow int16.
func AppendMatroskaFrame(b []byte, ms int64, frame []byte) []byte {
	if ms < 0 {
		ms = 0
	}
	var hdr [32]byte // cluster timestamp and the SimpleBlock's head, without the frame
	h := appendUint(hdr[:0], elTimecode, uint64(ms))
	h = appendID(h, elSimpleBlock)
	h = appendSize(h, uint64(4+len(frame)))
	h = append(h, 0x81, 0, 0, 0x80) // track 1, relative time 0, keyframe
	b = appendID(b, elCluster)
	b = appendSize(b, uint64(len(h)+len(frame)))
	return append(append(b, h...), frame...)
}
