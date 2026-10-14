package main

import "time"

func duration(seconds int) string {
	return (time.Duration(seconds) * time.Second).String()
}
