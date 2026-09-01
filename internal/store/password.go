package store

import "golang.org/x/crypto/bcrypt"

// HashPassword 生成口令的 bcrypt 哈希。
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验明文口令与 bcrypt 哈希是否匹配。
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// bcryptHash 兼容旧调用的内部别名。
func bcryptHash(pw string) (string, error) { return HashPassword(pw) }
