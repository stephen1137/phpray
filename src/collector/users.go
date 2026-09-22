package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UserResolver maps numeric UIDs to usernames using /etc/passwd
type UserResolver struct {
	mu       sync.RWMutex
	cache    map[uint32]string // UID → username
	lastLoad time.Time
	ttl      time.Duration
	passwdPath string
}

// NewUserResolver creates a resolver that reads /etc/passwd
func NewUserResolver() *UserResolver {
	r := &UserResolver{
		cache:      make(map[uint32]string),
		ttl:        5 * time.Minute,
		passwdPath: "/etc/passwd",
	}
	r.refresh()
	return r
}

// Resolve returns the username for a UID, refreshing cache if stale
func (r *UserResolver) Resolve(uid uint32) string {
	r.mu.RLock()
	if time.Since(r.lastLoad) > r.ttl {
		r.mu.RUnlock()
		r.refresh()
		r.mu.RLock()
	}
	name, ok := r.cache[uid]
	r.mu.RUnlock()

	if ok {
		return name
	}
	return fmt.Sprintf("uid:%d", uid)
}

// ResolveAll returns usernames for a batch of UIDs (efficient: single lock)
func (r *UserResolver) ResolveAll(uids []uint32) map[uint32]string {
	r.mu.RLock()
	if time.Since(r.lastLoad) > r.ttl {
		r.mu.RUnlock()
		r.refresh()
		r.mu.RLock()
	}
	defer r.mu.RUnlock()

	result := make(map[uint32]string, len(uids))
	for _, uid := range uids {
		if name, ok := r.cache[uid]; ok {
			result[uid] = name
		} else {
			result[uid] = fmt.Sprintf("uid:%d", uid)
		}
	}
	return result
}

// GetAll returns the full UID→username map (copy)
func (r *UserResolver) GetAll() map[uint32]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[uint32]string, len(r.cache))
	for k, v := range r.cache {
		result[k] = v
	}
	return result
}

// refresh reloads /etc/passwd
func (r *UserResolver) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Don't refresh if recently loaded (race protection)
	if time.Since(r.lastLoad) < r.ttl/2 {
		return
	}

	newCache := make(map[uint32]string)

	f, err := os.Open(r.passwdPath)
	if err != nil {
		log.Printf("UserResolver: cannot read %s: %v", r.passwdPath, err)
		r.lastLoad = time.Now()
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}

		// Format: username:x:uid:gid:gecos:home:shell
		parts := strings.SplitN(line, ":", 7)
		if len(parts) < 4 {
			continue
		}

		username := parts[0]
		uidStr := parts[2]

		uid64, err := strconv.ParseUint(uidStr, 10, 32)
		if err != nil {
			continue
		}
		uid := uint32(uid64)

		// Skip system users (uid < 500) except well-known ones
		// On DirectAdmin servers, hosting users start at uid 500+
		// But we cache all for completeness
		newCache[uid] = username
	}

	r.cache = newCache
	r.lastLoad = time.Now()

	if len(newCache) > 0 {
		// Count hosting users (uid >= 500, not nobody/nfsnobody)
		hostingUsers := 0
		for uid, name := range newCache {
			if uid >= 500 && name != "nobody" && name != "nfsnobody" {
				hostingUsers++
			}
		}
		log.Printf("UserResolver: loaded %d users (%d hosting users) from %s",
			len(newCache), hostingUsers, r.passwdPath)
	}
}

// IsHostingUser returns true if the UID looks like a hosting user (>= 500)
func IsHostingUser(uid uint32) bool {
	return uid >= 500
}

// ─── DomainResolver ──────────────────────────────────────────────────────────

// DomainOwner holds the owner info for a domain
type DomainOwner struct {
	Username string `json:"username"`
	UID      uint32 `json:"uid"`
}

// DomainResolver maps domain names to their hosting user owner by reading
// DirectAdmin's user data directory structure:
//   /usr/local/directadmin/data/users/<username>/domains.list
// Each file contains one domain per line.
type DomainResolver struct {
	mu       sync.RWMutex
	cache    map[string]*DomainOwner // domain → owner
	lastLoad time.Time
	ttl      time.Duration
	daPath   string // path to DirectAdmin data/users/ directory
	users    *UserResolver // for UID lookup
}

const defaultDAPath = "/usr/local/directadmin/data/users"

// NewDomainResolver creates a resolver that scans DirectAdmin user directories.
// daPath is the path to the DA users directory (empty = default).
// users is the UserResolver for UID lookup by username.
func NewDomainResolver(daPath string, users *UserResolver) *DomainResolver {
	if daPath == "" {
		daPath = defaultDAPath
	}
	r := &DomainResolver{
		cache:  make(map[string]*DomainOwner),
		ttl:    10 * time.Minute, // Domain ownership changes rarely
		daPath: daPath,
		users:  users,
	}
	r.refresh()
	return r
}

// Resolve returns the owner of a domain, or nil if unknown.
// Handles exact match and subdomain matching (e.g., www.example.com → example.com owner).
func (r *DomainResolver) Resolve(domain string) *DomainOwner {
	r.mu.RLock()
	if time.Since(r.lastLoad) > r.ttl {
		r.mu.RUnlock()
		r.refresh()
		r.mu.RLock()
	}
	defer r.mu.RUnlock()

	// Strip port if present (e.g., "example.com:8080")
	if idx := strings.IndexByte(domain, ':'); idx > 0 {
		domain = domain[:idx]
	}

	// Exact match first
	if owner, ok := r.cache[domain]; ok {
		return owner
	}

	// Try stripping subdomains: www.shop.example.com → shop.example.com → example.com
	parts := strings.Split(domain, ".")
	for i := 1; i < len(parts)-1; i++ {
		candidate := strings.Join(parts[i:], ".")
		if owner, ok := r.cache[candidate]; ok {
			return owner
		}
	}

	return nil
}

// ResolveMulti returns owners for multiple domains (efficient: single lock)
func (r *DomainResolver) ResolveMulti(domains []string) map[string]*DomainOwner {
	r.mu.RLock()
	if time.Since(r.lastLoad) > r.ttl {
		r.mu.RUnlock()
		r.refresh()
		r.mu.RLock()
	}
	defer r.mu.RUnlock()

	result := make(map[string]*DomainOwner, len(domains))
	for _, domain := range domains {
		d := domain
		if idx := strings.IndexByte(d, ':'); idx > 0 {
			d = d[:idx]
		}

		if owner, ok := r.cache[d]; ok {
			result[domain] = owner
			continue
		}

		// Subdomain fallback
		parts := strings.Split(d, ".")
		for i := 1; i < len(parts)-1; i++ {
			candidate := strings.Join(parts[i:], ".")
			if owner, ok := r.cache[candidate]; ok {
				result[domain] = owner
				break
			}
		}
	}
	return result
}

// GetAll returns a copy of the full domain→owner map
func (r *DomainResolver) GetAll() map[string]*DomainOwner {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*DomainOwner, len(r.cache))
	for k, v := range r.cache {
		result[k] = v
	}
	return result
}

// Count returns the number of mapped domains
func (r *DomainResolver) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// refresh scans DirectAdmin user directories for domains.list files
func (r *DomainResolver) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Don't refresh if recently loaded (race protection)
	if time.Since(r.lastLoad) < r.ttl/2 {
		return
	}

	newCache := make(map[string]*DomainOwner)

	// Check if DA path exists
	entries, err := os.ReadDir(r.daPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("DomainResolver: cannot read %s: %v", r.daPath, err)
		}
		// Not an error on non-DA servers — just no domain mapping available
		r.lastLoad = time.Now()
		return
	}

	// Build reverse username→UID map from UserResolver
	uidMap := make(map[string]uint32)
	if r.users != nil {
		allUsers := r.users.GetAll()
		for uid, username := range allUsers {
			uidMap[username] = uid
		}
	}

	userCount := 0
	domainCount := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		username := entry.Name()

		// Read domains.list
		domainsPath := r.daPath + "/" + username + "/domains.list"
		f, err := os.Open(domainsPath)
		if err != nil {
			continue // User may not have domains.list (e.g., reseller without domains)
		}

		uid, hasUID := uidMap[username]

		scanner := bufio.NewScanner(f)
		userHasDomains := false
		for scanner.Scan() {
			domain := strings.TrimSpace(scanner.Text())
			if domain == "" || domain[0] == '#' {
				continue
			}

			owner := &DomainOwner{
				Username: username,
				UID:      uid,
			}
			if !hasUID {
				// Try to get UID from system if not in cache
				// (might happen if passwd cache is stale)
				owner.UID = 0
			}

			newCache[domain] = owner
			domainCount++
			userHasDomains = true
		}
		f.Close()

		if userHasDomains {
			userCount++
		}
	}

	r.cache = newCache
	r.lastLoad = time.Now()

	if domainCount > 0 {
		log.Printf("DomainResolver: loaded %d domains from %d users (%s)",
			domainCount, userCount, r.daPath)
	}
}
