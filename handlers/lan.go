package handlers

import (
	"bufio"
	"fmt"

	"visualclassroom/classserver/utils"

	"github.com/gofiber/fiber/v3"
)

// IPChangeStream 返回 Server-Sent Events 流，推送 IP 变化。
func IPChangeStream(c fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	ch := utils.SubscribeIPChanges()
	defer utils.UnsubscribeIPChanges(ch)

	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		for msg := range ch {
			fmt.Fprintf(w, "data: %s\n\n", msg)
			w.Flush()
		}
	})
	return nil
}

// GetLANInfo 保留轮询备用
func GetLANInfo(c fiber.Ctx) error {
	ip, port := utils.GetCurrentLANInfo()
	return c.JSON(fiber.Map{
		"ip":   ip,
		"port": port,
	})
}
