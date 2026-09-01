package marker

import "testing"

func TestMissReturnsZeroWithoutInsert(t *testing.T) {
	s := NewSaltStore(8)
	if v := s.Get("mk-1"); v != 0 {
		t.Fatalf("miss should return 0, got %d", v)
	}
	if s.Len() != 0 {
		t.Fatalf("miss must not insert, len=%d", s.Len())
	}
}

func TestFailureThresholdAndSuccessReset(t *testing.T) {
	s := NewSaltStore(8)
	if rotated, salt, failures := s.RecordFailure("identity-1", 2); rotated || salt != 0 || failures != 1 {
		t.Fatalf("first failure = rotated:%v salt:%d failures:%d, want false/0/1", rotated, salt, failures)
	}
	s.ClearFailures("identity-1")
	if rotated, salt, failures := s.RecordFailure("identity-1", 2); rotated || salt != 0 || failures != 1 {
		t.Fatalf("failure after success reset = rotated:%v salt:%d failures:%d, want false/0/1", rotated, salt, failures)
	}
	if rotated, salt, failures := s.RecordFailure("identity-1", 2); !rotated || salt != 1 || failures != 2 {
		t.Fatalf("threshold crossing = rotated:%v salt:%d failures:%d, want true/1/2", rotated, salt, failures)
	}
}

func TestRotateIncrements(t *testing.T) {
	s := NewSaltStore(8)
	if v := s.Rotate("mk-1"); v != 1 {
		t.Fatalf("first rotate should be 1, got %d", v)
	}
	if v := s.Rotate("mk-1"); v != 2 {
		t.Fatalf("second rotate should be 2, got %d", v)
	}
	if v := s.Rotate("ak-2"); v != 1 {
		t.Fatalf("independent ak should start at 1, got %d", v)
	}
	if v := s.Get("mk-1"); v != 2 {
		t.Fatalf("Get after rotates should be 2, got %d", v)
	}
}

// LRU 淘汰顺序：Get/Rotate 均刷新最近使用；超容量淘汰最久未用条目。
func TestLRUEviction(t *testing.T) {
	s := NewSaltStore(2)
	s.Rotate("a")
	s.Rotate("b")
	if v := s.Get("a"); v != 1 { // 触摸 a → b 成为最久未用
		t.Fatalf("a should be 1, got %d", v)
	}
	s.Rotate("c") // 容量满，应淘汰 b

	if v := s.Get("b"); v != 0 {
		t.Fatalf("b should have been evicted (0), got %d", v)
	}
	if v := s.Get("a"); v != 1 {
		t.Fatalf("a should survive with salt 1, got %d", v)
	}
	if v := s.Get("c"); v != 1 {
		t.Fatalf("c should survive with salt 1, got %d", v)
	}
	if s.Len() != 2 {
		t.Fatalf("len must not exceed capacity, got %d", s.Len())
	}
}

func TestDefaultCapacityApplied(t *testing.T) {
	s := NewSaltStore(0) // <=0 回落到 DefaultCapacity
	for i := 0; i < DefaultCapacity+50; i++ {
		s.Rotate(string(rune('A'+i%26)) + "-" + itoa(i))
	}
	if s.Len() > DefaultCapacity {
		t.Fatalf("len %d exceeds default capacity %d", s.Len(), DefaultCapacity)
	}
}

func TestKeyIsHashedNotPlaintext(t *testing.T) {
	if keyOf("secret-ak") == "secret-ak" {
		t.Fatal("key must be a hash, never the plaintext Marker")
	}
	if keyOf("secret-ak") != keyOf("secret-ak") {
		t.Fatal("keyOf must be deterministic")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
