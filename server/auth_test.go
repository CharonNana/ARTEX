package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/golang-jwt/jwt/v5"
)

// GetSetting 对"键不存在"和"读取出错"的返回值只差一个 error：两种情况 value 都是
// 空串。密码相关的 handler 一旦把 error 当成"还没设置密码"，就会在数据库抖动期间
// 敞开初始化入口——authInit 会放行一个未认证请求去覆盖已有的管理员密码，
// authStatus 则会把前端直接送到 /setup 去照着做这件事。
//
// 这两个测试把 handler 的连接池关掉来制造读取失败，断言两处都 fail closed。
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// 关掉池子，让后续 GetSetting 返回 error 而不是 sql.ErrNoRows。
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (读失败被当成未初始化会把用户送去 /setup 覆盖密码); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("读失败时不应回答 initialized，得到 %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (读失败时放行会让未认证请求覆盖已有密码); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("读失败时不应签发 token: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"空", "", true},
		{"七位", "1234567", true},
		{"八位", "12345678", false},
		{"八个汉字按字符数而非字节数计", "密码密码密码密码", false},
		{"三个汉字够 9 字节但只有 3 个字符", "密码强", true},
		{"72 字节", strings.Repeat("a", 72), false},
		{"73 字节超出 bcrypt 上限", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}

func TestJWTAccountCompatibility(t *testing.T) {
	key := []byte("ctest-auth-test-signing-key-32-bytes")
	token, err := signJWT(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return key, nil })
	if err != nil || !parsed.Valid {
		t.Fatalf("new token invalid: %v", err)
	}
	if subject, _ := parsed.Claims.GetSubject(); subject != "CTEST" {
		t.Fatalf("subject=%q want CTEST", subject)
	}
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "ARTEX", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyJWT(legacy, key) || !verifyJWT(token, key) {
		t.Fatal("new and legacy sessions must both remain valid")
	}
	if verifyJWT(token, []byte("wrong-signing-key")) {
		t.Fatal("token signed by a different key must be rejected")
	}
}

func TestAuthAccountCompatibility(t *testing.T) {
	// 必须显式指定空的测试数据库，避免初始化或修改现有实例密码。
	dsn := os.Getenv("ARTEX_AUTH_TEST_DSN")
	if dsn == "" {
		t.Skip("set ARTEX_AUTH_TEST_DSN to an empty dedicated test database")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if hash, _, err := pg.GetSetting(authPassKey); err != nil || hash != "" {
		t.Fatalf("test requires an empty password setting: %v", err)
	}
	s := &Server{m: &Manager{pg: pg}, jwtKey: []byte("ctest-auth-test-signing-key-32-bytes")}
	password := "original-test-password"
	init := httptest.NewRecorder()
	s.authInit(init, httptest.NewRequest("POST", "/api/auth/init", strings.NewReader(`{"password":"`+password+`"}`)))
	if init.Code != 200 {
		t.Fatalf("init status=%d body=%s", init.Code, init.Body.String())
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(init.Body.Bytes(), &session); err != nil || !verifyJWT(session.Token, s.jwtKey) {
		t.Fatalf("init did not issue a valid session: %v", err)
	}
	originalHash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		username, password string
		status             int
	}{
		{"CTEST", password, 200}, {"ARTEX", password, 200},
		{"other", password, 401}, {"ctest", password, 401},
		{"CTEST", "wrong-password", 401}, {"ARTEX", "wrong-password", 401}, {"CTEST", "", 401},
	} {
		t.Run(tc.username+"/"+tc.password, func(t *testing.T) {
			w := httptest.NewRecorder()
			body, _ := json.Marshal(map[string]string{"username": tc.username, "password": tc.password})
			s.authLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(body))))
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 200 {
				var reply struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || !verifyJWT(reply.Token, s.jwtKey) {
					t.Fatalf("login did not issue a valid session: %v", err)
				}
			} else if strings.Contains(w.Body.String(), "token") {
				t.Fatal("failed login must not issue a token")
			}
		})
	}
	if hash, _, err := pg.GetSetting(authPassKey); err != nil || hash != originalHash {
		t.Fatal("login must preserve the existing password hash", err)
	}
	change := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/auth/change-password", strings.NewReader(`{"old_password":"`+password+`","new_password":"updated-test-password"}`))
	r.Header.Set("Authorization", "Bearer "+session.Token)
	s.authChangePassword(change, r)
	if change.Code != 200 {
		t.Fatalf("change password status=%d body=%s", change.Code, change.Body.String())
	}
	for _, username := range []string{"CTEST", "ARTEX"} {
		w := httptest.NewRecorder()
		s.authLogin(w, httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"`+username+`","password":"updated-test-password"}`)))
		if w.Code != 200 {
			t.Fatalf("%s cannot log in with updated password: %d", username, w.Code)
		}
	}
	if !verifyJWT(session.Token, s.jwtKey) {
		t.Fatal("password change must preserve an already issued session")
	}
}
