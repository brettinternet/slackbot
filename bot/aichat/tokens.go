package aichat

import (
	"strings"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// OpenAI chat messages use three framing tokens for the role and message
// envelope, in addition to the encoded content.
const chatMessageFramingTokens = 3

type tokenCounter struct {
	encoding    *tiktoken.Tiktoken
	initialized bool
}

func newTokenCounter(model string) tokenCounter {
	var (
		encoding *tiktoken.Tiktoken
		err      error
	)

	// tiktoken-go predates these model families and otherwise matches some of
	// them to the older cl100k encoding through the broad "gpt-4" prefix.
	switch {
	case strings.HasPrefix(model, "gpt-4o"),
		strings.HasPrefix(model, "gpt-4.1"),
		strings.HasPrefix(model, "gpt-4.5"),
		strings.HasPrefix(model, "gpt-5"),
		strings.HasPrefix(model, "o1"),
		strings.HasPrefix(model, "o3"),
		strings.HasPrefix(model, "o4"):
		encoding, err = tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
	default:
		encoding, err = tiktoken.EncodingForModel(model)
	}
	if err != nil {
		return tokenCounter{initialized: true}
	}
	return tokenCounter{encoding: encoding, initialized: true}
}

func (c Config) modelTokenCounter() tokenCounter {
	if c.tokenCounter.initialized {
		return c.tokenCounter
	}
	return newTokenCounter(c.Model)
}

func (c tokenCounter) messageTokens(text string) int {
	if c.encoding == nil {
		// One token per UTF-8 byte is a conservative upper bound for byte-level
		// tokenizers when a model has no known tokenizer mapping.
		return len(text) + chatMessageFramingTokens
	}
	return len(c.encoding.EncodeOrdinary(text)) + chatMessageFramingTokens
}
