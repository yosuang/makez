SHELL := /bin/bash

.PHONY: dev clean

dev:
	./dev/dev.sh

clean:
	go run ./build clean
