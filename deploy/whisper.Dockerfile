# Supply an existing whisper.cpp build image with its sources and Intel runtime.
ARG WHISPER_IMAGE
FROM ${WHISPER_IMAGE}
USER root
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
# The older server converter downmixes stereo before its diarizer can read it.
RUN grep -q -- '-ac 1 ' /whisper.cpp/examples/server/server.cpp && sed -i 's/-ac 1 //g' /whisper.cpp/examples/server/server.cpp && ! grep -q -- '-ac 1 ' /whisper.cpp/examples/server/server.cpp && /bin/bash -c '. /opt/intel/oneapi/setvars.sh --force >/dev/null && cmake --build /whisper.cpp/build --target whisper-server -j4'
RUN mkdir /podcast && chmod 777 /podcast
WORKDIR /podcast
USER 65532:65532
ENTRYPOINT ["/whisper.cpp/build/bin/whisper-server"]
