package main

import "compound/internal/work/executewire"

func init() { executewire.Install(); executewire.InstallDetached() }
