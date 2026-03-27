set CGO_ENABLED=1
set GOOS=android
set ANDROID_NDK_HOME=C:/Sdk/android-sdk/ndk/25.1.8937393

set CC=%ANDROID_NDK_HOME%/toolchains/llvm/prebuilt/windows-x86_64/bin/x86_64-linux-android21-clang
set GOARCH=amd64
go build -buildmode=c-shared -o x86_64/libgo.so

set CC=%ANDROID_NDK_HOME%/toolchains/llvm/prebuilt/windows-x86_64/bin/aarch64-linux-android21-clang
set GOARCH=arm64
go build -buildmode=c-shared -o arm64-v8a/libgo.so