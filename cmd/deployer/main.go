package main

import (
	"fmt"

	"github.com/Aegis-AI-Organizations/aegis-ai-worker-deployer/internal/deployer"
)

func main() {
	fmt.Println("Hello from Aegis AI Worker Deployer!")
	deployer.Start()
}
