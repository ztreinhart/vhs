#!/bin/bash

go run echo.go &

while true; do 
    curl -s -d "hello, world $(date +%s)" -w "\n" 0.0.0.0:1111
    sleep 1
done
