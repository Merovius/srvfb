# srvfb - Stream framebuffer content over HTTP

This repository contains a small webserver that can serve the contents of a
linux framebuffer device as video over HTTP. The video is encoded as a series
of PNGs, which are served in a `multipart/x-mixed-replace` stream. The primary
use case is to stream a [reMarkable][reMarkable] screen to a computer and share
it from there via video-conferencing or capturing it. For that reason, there is
also a proxy-mode, which streams the frames as raw, uncompressed data from the
remarkable and can then do the png-encoding on a more powerful machine.
Whithout that, the framerate is one or two frames per second, which might not
be acceptable (it might be, though).

This should be considered a tech demo in the current state. The code is not
particularly clean, it's not in any way secured, probably not very efficient
and it's taylored specifically to the reMarkable (e.g. it can only stream
16-bit grayscale images). Feel free to use it and report any bugs you find, but
I don't make any promises in regards to support or stability and any issues not
directly related to my usecase will likely be closed.

You can see a short video demonstrating this [in this tweet][video]

# Installation and usage

You need a working [Go installation][go] and [ssh-access to your reMarkable][ssh].
You can then obtain, install and run the code via

```
go get -d -u github.com/Merovius/srvfb
GOARCH=arm GOOS=linux go build github.com/Merovius/srvfb
scp srvfb root@10.11.99.1:
ssh root@10.11.99.1 ./srvfb -device /dev/fb0
```

If you then open `http://10.11.99.1:1234/video` in your browser (only Chrome
is tested) you should see the stream from your reMarkable. To use proxy-mode,
run (in a separate terminal)

```
go build github.com/Merovius/srvfb
./srvfb -listen localhost:1234 -proxy 10.11.99.1
```

and open `http://localhost:1234/video` in your browser.

This repository also contains systemd unit files to run `srvfb` automatically
(using socket activation). For security reasons, it only listens on the USB
network, though. To use it, run

```
cd $(go env GOPATH)/src/github.com/Merovius/srvfb
GOARCH=arm GOOS=linux go build github.com/Merovius/srvfb
scp srvfb root@10.11.99.1:/usr/bin
scp contrib/srvfb.service contrib/srvfb.socket root@10.11.99.1:/etc/systemd/system
ssh root@10.11.99.1 systemctl enable --now srvfb.socket
```

# License

Apart where otherwise noted, this code is published under the Apache License,
Version 2.0:

```
Copyright 2018 Axel Wagner

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```

[reMarkable]: https://remarkable.com/
[go]: https://golang.org/doc/install
[ssh]: https://remarkablewiki.com/tech/ssh
[video]: https://twitter.com/TheMerovius/status/1066455790117097472
