SHELL := /bin/bash

.PHONY: dev clean

dev:
	./build/dev.sh

clean:
	go run ./build clean
