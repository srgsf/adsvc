# Labelled benchmark

`adsvc bench` scores the detector against ground truth: where the library's ads really occur
in real files. The media is not in the repository; paths in a labels file are relative to it.

```sh
./adsvc bench -labels bench/babylon-s05.json                       # offline: ffmpeg decodes each file
./adsvc bench -labels bench/babylon-s05.json -degrade aac48 -only 1080p
./adsvc bench -labels bench/babylon-s05.json -mode proxy -ffmpeg dist/ffmpeg
./adsvc bench -labels bench/babylon-s05.json -json run.json        # keep a run to compare with later
```

* **offline** scans each file from the start, with the proxy's confirmation rule, and also measures
  the *look-ahead*: how much audio past the ad's start had to be decoded before the ad was
  detected / confirmed (what a player must have buffered to act on it from its first frame),
  and the *noise*: the strongest candidate that is not a labelled occurrence.
* **proxy** streams each file through a real `Proxy` (container demuxers, minimal ffmpeg) in
  one read, never dropping data, and scores the ad map it stores.
* **-degrade** (offline) transforms the programme audio first: `aac48` / `mp3-32` (low-bitrate
  mono re-encode), `pal` (23.976→25 fps speed-up, pitch up), `pal-tempo` (the same,
  pitch-corrected). Labels are rescaled to the sped-up timeline.

## Labels

```json
{
  "data_dir": "..",
  "files": [
    {"paths": ["ep1.mkv", "ep1.avi"],
     "ads": [{"ad": "38b12ce7-a9be-a6cf-dc79-f4d844fea034", "startMs": 270270, "endMs": 300260,
              "source": "manual", "verified": true}]}
  ]
}
```

`data_dir` is the data directory whose catalogue the ad ids refer to (`-data-dir` overrides
it). Times are integer milliseconds on the file's media timeline.

A file lists every release/container with the same audio timeline. `source` says where the
boundaries come from: `manual` (set by hand; the only kind boundary error is measured
against), `enrolled` (the segment the ad was enrolled from), `detected` (bootstrapped).
`verified` means a person checked the ad is really there.

`./adsvc bench -draft out.json files...` writes a draft from what the detector finds, every
entry `detected` and unverified. **A draft is circular**: scored against itself it gives 100%
recall at the threshold it was drafted with. It is still a baseline for fingerprint changes,
degradations and the proxy path, but recall on clean audio only means something once a person
has watched the files, confirmed each entry and added the occurrences the detector missed.

## babylon-s05.json

9 episodes (1080p MKV + 400p AVI, the same AC-3 384k stereo track), 10 ads, 27 occurrences.
To verify an occurrence, play it through the proxy and seek a few seconds before `startMs`;
for a manual boundary, frame-step in mpv and set `startMs`/`endMs` and `"source": "manual"`.
