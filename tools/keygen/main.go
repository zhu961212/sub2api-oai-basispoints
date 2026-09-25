// Command keygen 生成用于给 .s2plugin 签名的 Ed25519 密钥对。
//
//	go run ./tools/keygen -out build/keys/my-publisher
//
// 生成 <out>.private（base64，仅保存在受控环境）与 <out>.public（base64，交给部署者写入
// Sub2API 配置的 plugins.trusted_publishers）。私钥绝不能提交到仓库或放进插件包。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "build/keys/publisher", "密钥文件前缀")
	flag.Parse()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail(err)
	}
	privatePath := *out + ".private"
	publicPath := *out + ".public"
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(privateKey)+"\n"), 0o600); err != nil {
		fail(err)
	}
	if err := os.WriteFile(publicPath, []byte(base64.StdEncoding.EncodeToString(publicKey)+"\n"), 0o644); err != nil {
		fail(err)
	}

	fmt.Printf("私钥（保密，勿提交）: %s\n", privatePath)
	fmt.Printf("公钥: %s\n", publicPath)
	fmt.Printf("公钥 base64: %s\n", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Println("\n部署者需要在 Sub2API 配置中追加：")
	fmt.Println("plugins:")
	fmt.Println("  allow_unsigned: false")
	fmt.Println("  trusted_publishers:")
	fmt.Printf("    <key-id>: \"%s\"\n", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Println("\n打包时使用同一个 <key-id>：")
	fmt.Printf("  go run ./tools/packager -signing-key %s -key-id <key-id>\n", privatePath)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "keygen:", err)
	os.Exit(1)
}
