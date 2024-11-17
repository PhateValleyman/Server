#!/data/data/com.termux/files/usr/bin/sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=5 go build -v -x -mod=vendor -trimpath -o ./bin/server ./main.go
