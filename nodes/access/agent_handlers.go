package main

import "fmt"

// mustMergeHandlers 只用于静态接线；不同协议组的重复 ID 必须在构造 Dispatcher 前暴露。
func mustMergeHandlers[ID ~uint32](groups ...map[ID]any) map[ID]any {
	handlers := make(map[ID]any)
	for _, group := range groups {
		for id, handler := range group {
			if _, exists := handlers[id]; exists {
				panic(fmt.Sprintf("access: duplicate handler ID %d", id))
			}
			handlers[id] = handler
		}
	}
	return handlers
}
