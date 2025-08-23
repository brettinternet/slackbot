package aichat

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"go.uber.org/zap"
)

func TestNewContextStorage(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()
	
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()
	
	// Check that database file was created
	dbPath := filepath.Join(tempDir, "aichat_context.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("Database file was not created at %s", dbPath)
	}
}

func TestContextStorage_StoreAndRetrieve(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewContextStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create context storage: %v", err)
	}
	defer func() { _ = storage.Close() }()
	
	// Store some test context
	testContext := ConversationContext{
		UserID:      "U123",
		ChannelID:   "C456",
		PersonaName: "test",
		Message:     "Hello, world!",
		Role:        "human",
		Timestamp:   time.Now(),
	}
	
	err = storage.StoreContext(testContext)
	if err != nil {
		t.Fatalf("Failed to store context: %v", err)
	}
	
	// Retrieve the context
	contexts, err := storage.GetRecentContext("U123", "C456", "test", 10)
	if err != nil {
		t.Fatalf("Failed to retrieve context: %v", err)
	}
	
	if len(contexts) != 1 {
		t.Errorf("Expected 1 context, got %d", len(contexts))
	}
	
	if contexts[0].Message != "Hello, world!" {
		t.Errorf("Expected message 'Hello, world!', got '%s'", contexts[0].Message)
	}
}

func TestAIChat_RandomPersonaName(t *testing.T) {
	logger := zap.NewNop()
	
	// Test with configured personas
	config := Config{
		Personas: map[string]string{
			"test1": "Test persona 1",
			"test2": "Test persona 2",
		},
		StickyDuration: 30 * time.Minute,
	}
	
	aiChat := &AIChat{
		log:    logger,
		config: config,
	}
	
	// Should return one of the configured personas
	persona := aiChat.randomPersonaName()
	if persona != "test1" && persona != "test2" {
		t.Errorf("Expected 'test1' or 'test2', got '%s'", persona)
	}
	
	// Test with empty personas (should return default)
	emptyConfig := Config{
		Personas:       map[string]string{},
		StickyDuration: 30 * time.Minute,
	}
	aiChatEmpty := &AIChat{
		log:    logger,
		config: emptyConfig,
	}
	
	persona = aiChatEmpty.randomPersonaName()
	if persona != "default" {
		t.Errorf("Expected 'default', got '%s'", persona)
	}
}

func TestConfig_PersonasParsing(t *testing.T) {
	// Test YAML parsing
	yamlConfig := `
glazer: "Gen-Z hype beast persona"
argue: "Argumentative lawyer persona"
`
	
	personasData := make(map[string]interface{})
	err := yaml.Unmarshal([]byte(yamlConfig), &personasData)
	if err != nil {
		t.Fatalf("Failed to parse YAML config: %v", err)
	}
	
	if len(personasData) != 2 {
		t.Errorf("Expected 2 personas, got %d", len(personasData))
	}
	
	if personasData["glazer"] != "Gen-Z hype beast persona" {
		t.Errorf("Unexpected glazer persona: %v", personasData["glazer"])
	}
}