package main

import "strings"

func catalogModelEntry(m gatewayModel) map[string]any {
	if isLoomyImageModel(m.ID) && strings.EqualFold(m.Type, "image") {
		return imageModelEntry(m)
	}
	return modelEntry(m.ID, m.ContextLength, m.Capabilities.Vision, m.Name, m.ReasoningEfforts...)
}
