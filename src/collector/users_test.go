package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDomainResolver_Basic(t *testing.T) {
	// Create temp DirectAdmin-like directory structure
	tmpDir := t.TempDir()

	// User "alice" with 2 domains
	aliceDir := filepath.Join(tmpDir, "alice")
	os.MkdirAll(aliceDir, 0755)
	os.WriteFile(filepath.Join(aliceDir, "domains.list"), []byte("alice.com\nshop.alice.com\n"), 0644)

	// User "bob" with 1 domain
	bobDir := filepath.Join(tmpDir, "bob")
	os.MkdirAll(bobDir, 0755)
	os.WriteFile(filepath.Join(bobDir, "domains.list"), []byte("bob-site.net\n"), 0644)

	// User "charlie" with no domains.list
	charlieDir := filepath.Join(tmpDir, "charlie")
	os.MkdirAll(charlieDir, 0755)

	// Create a UserResolver with a custom passwd file
	passwdPath := filepath.Join(tmpDir, "passwd")
	os.WriteFile(passwdPath, []byte(
		"alice:x:1001:1001::/home/alice:/bin/bash\n"+
			"bob:x:1002:1002::/home/bob:/bin/bash\n"+
			"charlie:x:1003:1003::/home/charlie:/bin/bash\n",
	), 0644)

	users := &UserResolver{
		cache:      map[uint32]string{1001: "alice", 1002: "bob", 1003: "charlie"},
		passwdPath: passwdPath,
	}

	resolver := NewDomainResolver(tmpDir, users)

	// Test exact match
	owner := resolver.Resolve("alice.com")
	if owner == nil {
		t.Fatal("Expected owner for alice.com, got nil")
	}
	if owner.Username != "alice" {
		t.Errorf("Expected username alice, got %s", owner.Username)
	}
	if owner.UID != 1001 {
		t.Errorf("Expected UID 1001, got %d", owner.UID)
	}

	// Test another domain
	owner = resolver.Resolve("bob-site.net")
	if owner == nil {
		t.Fatal("Expected owner for bob-site.net, got nil")
	}
	if owner.Username != "bob" {
		t.Errorf("Expected username bob, got %s", owner.Username)
	}

	// Test subdomain matching (www.alice.com → alice.com)
	owner = resolver.Resolve("www.alice.com")
	if owner == nil {
		t.Fatal("Expected owner for www.alice.com via subdomain match, got nil")
	}
	if owner.Username != "alice" {
		t.Errorf("Expected username alice for www.alice.com, got %s", owner.Username)
	}

	// Test unknown domain
	owner = resolver.Resolve("unknown.org")
	if owner != nil {
		t.Errorf("Expected nil for unknown.org, got %+v", owner)
	}

	// Test port stripping
	owner = resolver.Resolve("alice.com:8080")
	if owner == nil {
		t.Fatal("Expected owner for alice.com:8080, got nil")
	}
	if owner.Username != "alice" {
		t.Errorf("Expected username alice for alice.com:8080, got %s", owner.Username)
	}

	// Test Count
	count := resolver.Count()
	if count != 3 { // alice.com, shop.alice.com, bob-site.net
		t.Errorf("Expected 3 domains, got %d", count)
	}

	// Test GetAll
	all := resolver.GetAll()
	if len(all) != 3 {
		t.Errorf("Expected 3 domains in GetAll, got %d", len(all))
	}
}

func TestDomainResolver_NonExistentPath(t *testing.T) {
	resolver := NewDomainResolver("/nonexistent/path/that/should/not/exist", nil)
	
	if resolver.Count() != 0 {
		t.Errorf("Expected 0 domains for nonexistent path, got %d", resolver.Count())
	}

	owner := resolver.Resolve("anything.com")
	if owner != nil {
		t.Errorf("Expected nil owner for nonexistent path, got %+v", owner)
	}
}

func TestDomainResolver_ResolveMulti(t *testing.T) {
	tmpDir := t.TempDir()

	aliceDir := filepath.Join(tmpDir, "alice")
	os.MkdirAll(aliceDir, 0755)
	os.WriteFile(filepath.Join(aliceDir, "domains.list"), []byte("alice.com\n"), 0644)

	bobDir := filepath.Join(tmpDir, "bob")
	os.MkdirAll(bobDir, 0755)
	os.WriteFile(filepath.Join(bobDir, "domains.list"), []byte("bob.net\n"), 0644)

	users := &UserResolver{
		cache: map[uint32]string{1001: "alice", 1002: "bob"},
	}

	resolver := NewDomainResolver(tmpDir, users)

	results := resolver.ResolveMulti([]string{"alice.com", "bob.net", "unknown.org"})
	
	if len(results) != 2 {
		t.Errorf("Expected 2 results, got %d", len(results))
	}
	if results["alice.com"] == nil || results["alice.com"].Username != "alice" {
		t.Error("Expected alice.com → alice")
	}
	if results["bob.net"] == nil || results["bob.net"].Username != "bob" {
		t.Error("Expected bob.net → bob")
	}
	if _, ok := results["unknown.org"]; ok {
		t.Error("Expected unknown.org to not be in results")
	}
}

func TestDomainResolver_SubdomainChain(t *testing.T) {
	tmpDir := t.TempDir()

	aliceDir := filepath.Join(tmpDir, "alice")
	os.MkdirAll(aliceDir, 0755)
	os.WriteFile(filepath.Join(aliceDir, "domains.list"), []byte("example.com\n"), 0644)

	users := &UserResolver{
		cache: map[uint32]string{1001: "alice"},
	}

	resolver := NewDomainResolver(tmpDir, users)

	// Deep subdomain: blog.shop.example.com → example.com
	owner := resolver.Resolve("blog.shop.example.com")
	if owner == nil {
		t.Fatal("Expected owner for blog.shop.example.com, got nil")
	}
	if owner.Username != "alice" {
		t.Errorf("Expected alice, got %s", owner.Username)
	}
}
