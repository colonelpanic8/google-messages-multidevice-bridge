package whatsapp

import (
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func chatSupported(j types.JID) bool {
	switch normalizeJID(j).Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return j.User != ""
	}
	return false
}

// Only known user content creates messages. Protocol updates are handled separately.
func displayable(m *waE2E.Message) bool {
	return m != nil && (m.GetConversation() != "" || m.ExtendedTextMessage != nil ||
		m.ImageMessage != nil || m.VideoMessage != nil || m.AudioMessage != nil || m.DocumentMessage != nil || m.StickerMessage != nil ||
		m.ContactMessage != nil || m.ContactsArrayMessage != nil || m.LocationMessage != nil || m.LiveLocationMessage != nil ||
		m.TemplateMessage != nil || m.InteractiveMessage != nil || m.InteractiveResponseMessage != nil || m.HighlyStructuredMessage != nil ||
		m.ButtonsMessage != nil || m.ButtonsResponseMessage != nil || m.TemplateButtonReplyMessage != nil || m.ListMessage != nil || m.ListResponseMessage != nil ||
		m.PollCreationMessage != nil || m.PollCreationMessageV2 != nil || m.PollCreationMessageV3 != nil ||
		m.GroupInviteMessage != nil || m.ProductMessage != nil || m.OrderMessage != nil || m.EventMessage != nil)
}

func displayText(m *waE2E.Message) string {
	switch {
	case m.GetConversation() != "":
		return m.GetConversation()
	case m.ExtendedTextMessage != nil:
		return m.ExtendedTextMessage.GetText()
	case m.ContactMessage != nil:
		return "Contact: " + m.ContactMessage.GetDisplayName()
	case m.ContactsArrayMessage != nil:
		return "Contacts"
	case m.LocationMessage != nil:
		return "Location: " + m.LocationMessage.GetName()
	case m.LiveLocationMessage != nil:
		return "Live location"
	case m.PollCreationMessage != nil:
		return "Poll: " + m.PollCreationMessage.GetName()
	case m.PollCreationMessageV2 != nil:
		return "Poll: " + m.PollCreationMessageV2.GetName()
	case m.PollCreationMessageV3 != nil:
		return "Poll: " + m.PollCreationMessageV3.GetName()
	case m.TemplateMessage != nil:
		if text := m.TemplateMessage.GetHydratedFourRowTemplate().GetHydratedContentText(); text != "" {
			return text
		}
		if text := m.TemplateMessage.GetHydratedTemplate().GetHydratedContentText(); text != "" {
			return text
		}
		return "Template message"
	case m.InteractiveMessage != nil:
		if text := m.InteractiveMessage.GetBody().GetText(); text != "" {
			return text
		}
		return "Interactive message"
	case m.InteractiveResponseMessage != nil:
		return "Interactive response"
	case m.ButtonsMessage != nil:
		return m.ButtonsMessage.GetContentText()
	case m.ButtonsResponseMessage != nil:
		return m.ButtonsResponseMessage.GetSelectedDisplayText()
	case m.TemplateButtonReplyMessage != nil:
		return m.TemplateButtonReplyMessage.GetSelectedDisplayText()
	case m.ListMessage != nil:
		return m.ListMessage.GetDescription()
	case m.ListResponseMessage != nil:
		return m.ListResponseMessage.GetTitle()
	case m.GroupInviteMessage != nil:
		return "Group invitation: " + m.GroupInviteMessage.GetGroupName()
	case m.ProductMessage != nil:
		return "Product"
	case m.OrderMessage != nil:
		return "Order"
	case m.EventMessage != nil:
		return "Event: " + m.EventMessage.GetName()
	case m.HighlyStructuredMessage != nil:
		return "Structured message"
	}
	return ""
}
