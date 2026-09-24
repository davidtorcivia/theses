# Local transcription

Set `THESES_WHISPER_URL` to an operator-controlled Whisper.cpp `/inference` HTTP endpoint. The app sends recording bytes, an empty prompt, English language, and requests WebVTT. No hosted transcription API or billing key is used. Processing still consumes local electricity and GPU/CPU capacity.

Use a dedicated service for long podcast jobs if the existing Whisper service also serves interactive devices. `docker-compose.transcription.yml` adds a private service with no published port, four CPU cores, an 8 GiB memory limit and one app worker. Supply `THESES_WHISPER_MODELS`, `THESES_RENDER_GID`, and optionally `THESES_WHISPER_IMAGE`. The existing `ggml-large-v3-turbo-q5_0.bin` model is mounted read-only.

`deploy/whisper.Dockerfile` extends an existing Intel Whisper.cpp build image supplied through the `WHISPER_IMAGE` build argument. It expects sources/build artifacts in `/whisper.cpp` and the Intel toolchain in `/opt/intel/oneapi`. It installs FFmpeg and rebuilds the server's converter to preserve stereo, then runs as a non-root user. This recipe is specific to that existing runtime; other installations can point the app at their own compatible endpoint.

```sh
docker build --build-arg WHISPER_IMAGE=your-existing-whisper-image -f deploy/whisper.Dockerfile -t theses-whisper:local .
docker compose -f docker-compose.yml -f docker-compose.transcription.yml up -d
```

Jobs are explicitly requested in a ready recording's Transcript section. Up to ten jobs can queue; one runs at a time. A further request is refused until one finishes, and a recording with a queued or running job cannot be queued again. Requests time out after two hours and recordings larger than 1 GiB are refused. Interrupted jobs require explicit retry after restart. Disabling the service marks queued jobs failed as well, so they can be retried after reconfiguration. The worker rechecks the requesting user's access before processing and saving; a changed transcript is never overwritten by a stale job.

Stereo speaker labels require each speaker to occupy a separate channel. They are channel estimates, not identity recognition or diarization of a mixed mono conversation. Mono and mixed recordings can use manually edited speaker labels, or import speaker-tagged VTT from another tool. Plain text and SRT imports work without any inference service. TXT and VTT exports can be used in the host's transcript workflow.

Pinecast publishing stays in the Pinecast dashboard. When no legacy Transistor integration is configured, proposition settings provide a Pinecast handoff instead of inactive Transistor controls.

The app spool is a 1.25 GiB tmpfs in the optional Compose configuration, separate from the inference container's temporary audio. Interrupted requests cannot accumulate unbounded spool files on the app container's writable disk. Restores cancel and drain the worker before swapping the database and mark any restored running jobs interrupted. Transcript data, activity, and successful job completion commit in one transaction.

The tested existing Whisper.cpp runtime used source revision `8a9ad7844d6e2a10cddf4b92de4089d7ac2b14a9`. The build asserts that the old downmix flag is present before removing it and absent afterward. An incompatible base image fails the build instead of silently advertising stereo support.
