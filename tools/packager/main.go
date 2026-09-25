// Command packager 把插件工程打包成 Sub2API 的 .s2plugin。
//
// 它做四件事：
//  1. 交叉编译每个目标平台的运行时二进制；
//  2. 计算 runtimes/ 与 ui/ 下所有文件的 SHA-256，填充 manifest.json；
//  3. 可选地用 Ed25519 私钥对 manifest.json 的精确字节签名；
//  4. 按标准布局打成 ZIP（.s2plugin），并自检清单与哈希。
//
// 用法：
//
//	go run ./tools/packager -output dist/oai-basispoints-0.2.0.s2plugin
//	go run ./tools/packager -targets windows/amd64 -signing-key /secure/key.private -key-id my-publisher-v1
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultSource    = "manifest.source.json"
	defaultDist      = "dist"
	defaultTargets   = "windows/amd64,linux/amd64"
	defaultPackage   = "./cmd/oai-basispoints"
	binaryStem       = "oai-basispoints"
	uiDir            = "ui"
	runtimesDir      = "runtimes"
	manifestFile     = "manifest.json"
	signatureFile    = "signature.json"
	signatureAlg     = "ed25519"
	pluginPackageExt = ".s2plugin"
)

// Manifest 只声明打包器需要读写的字段；其余字段由 manifest.source.json 提供。
type Manifest struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Version       string                  `json:"version"`
	Description   string                  `json:"description,omitempty"`
	Author        string                  `json:"author,omitempty"`
	Requires      map[string]any          `json:"requires"`
	Capabilities  []map[string]string     `json:"capabilities"`
	Runtimes      map[string]RuntimeEntry `json:"runtimes"`
	UI            UIManifest              `json:"ui"`
	Files         map[string]string       `json:"files"`
}

type RuntimeEntry struct {
	Path string `json:"path"`
}

type UIManifest struct {
	Entrypoint string `json:"entrypoint"`
}

type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

type target struct {
	GOOS   string
	GOARCH string
}

func (t target) key() string { return t.GOOS + "-" + t.GOARCH }

func (t target) binaryPath() string {
	name := binaryStem
	if t.GOOS == "windows" {
		name += ".exe"
	}
	return runtimesDir + "/" + t.key() + "/" + name
}

func main() {
	sourcePath := flag.String("source", defaultSource, "插件清单源文件")
	distDir := flag.String("dist", defaultDist, "构建产物目录")
	uiSource := flag.String("ui", uiDir, "配置 UI 源目录")
	targetsFlag := flag.String("targets", defaultTargets, "目标平台，逗号分隔，例如 windows/amd64,linux/amd64")
	output := flag.String("output", "", "输出 .s2plugin 路径，默认 dist/<id>-<version>.s2plugin")
	packagePath := flag.String("package", defaultPackage, "运行时的 Go 包路径")
	signingKey := flag.String("signing-key", "", "Ed25519 私钥文件（base64），留空则产出未签名包")
	keyID := flag.String("key-id", "", "签名密钥 ID，必须与部署配置 trusted_publishers 的键一致")
	skipBuild := flag.Bool("skip-build", false, "跳过编译，复用 dist/runtimes 下已有二进制")
	flag.Parse()

	if err := run(*sourcePath, *distDir, *uiSource, *targetsFlag, *output, *packagePath, *signingKey, *keyID, *skipBuild); err != nil {
		fmt.Fprintln(os.Stderr, "packager:", err)
		os.Exit(1)
	}
}

func run(sourcePath, distDir, uiSource, targetsFlag, output, packagePath, signingKey, keyID string, skipBuild bool) error {
	manifest, err := readManifest(sourcePath)
	if err != nil {
		return err
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	targets, err := parseTargets(targetsFlag)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	if !skipBuild {
		for _, item := range targets {
			if err := buildRuntime(distDir, packagePath, item); err != nil {
				return err
			}
		}
	}
	if err := fillHashes(distDir, uiSource, manifest, targets); err != nil {
		return err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化清单: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')

	var signatureBytes []byte
	if strings.TrimSpace(signingKey) != "" {
		signatureBytes, err = signManifest(manifestBytes, signingKey, keyID)
		if err != nil {
			return err
		}
	}

	if strings.TrimSpace(output) == "" {
		output = filepath.Join(distDir, fmt.Sprintf("%s-%s%s", manifest.ID, manifest.Version, pluginPackageExt))
	}
	if err := writePackage(output, distDir, uiSource, manifestBytes, signatureBytes); err != nil {
		return err
	}
	if err := verifyPackage(output, manifestBytes); err != nil {
		return err
	}

	kind := "unsigned"
	if signatureBytes != nil {
		kind = "signed"
	}
	fmt.Printf("%s package written: %s\n", kind, output)
	fmt.Printf("plugin: %s %s (%d runtimes, %d files)\n", manifest.ID, manifest.Version, len(manifest.Runtimes), len(manifest.Files))
	if signatureBytes == nil {
		fmt.Println("note: production hosts reject unsigned packages; re-run with -signing-key and -key-id.")
	}
	return nil
}

func readManifest(sourcePath string) (*Manifest, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("读取清单源文件: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("解析清单源文件: %w", err)
	}
	if manifest.Runtimes == nil {
		manifest.Runtimes = map[string]RuntimeEntry{}
	}
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	return &manifest, nil
}

func validateManifest(manifest *Manifest) error {
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("schema_version 必须是 1，当前为 %d", manifest.SchemaVersion)
	}
	if strings.TrimSpace(manifest.ID) == "" || strings.TrimSpace(manifest.Version) == "" {
		return errors.New("清单必须声明 id 与 version")
	}
	if len(manifest.Capabilities) == 0 {
		return errors.New("清单必须声明至少一个能力")
	}
	if entrypoint := manifest.UI.Entrypoint; !strings.HasPrefix(entrypoint, uiDir+"/") {
		return fmt.Errorf("ui.entrypoint 必须位于 %s/ 目录，当前为 %q", uiDir, entrypoint)
	}
	return nil
}

func parseTargets(value string) ([]target, error) {
	items := strings.Split(value, ",")
	targets := make([]target, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("目标平台格式必须是 <goos>/<goarch>，当前为 %q", item)
		}
		entry := target{GOOS: parts[0], GOARCH: parts[1]}
		if seen[entry.key()] {
			continue
		}
		seen[entry.key()] = true
		targets = append(targets, entry)
	}
	if len(targets) == 0 {
		return nil, errors.New("至少需要一个目标平台")
	}
	return targets, nil
}

func buildRuntime(distDir, packagePath string, item target) error {
	outputPath := filepath.Join(distDir, filepath.FromSlash(item.binaryPath()))
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("创建运行时目录: %w", err)
	}
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", outputPath, packagePath)
	command.Env = append(os.Environ(), "GOOS="+item.GOOS, "GOARCH="+item.GOARCH, "CGO_ENABLED=0")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("编译 %s: %w", item.key(), err)
	}
	return nil
}

func fillHashes(distDir, uiSource string, manifest *Manifest, targets []target) error {
	files := map[string]string{}
	for _, item := range targets {
		path := item.binaryPath()
		hash, err := hashFile(filepath.Join(distDir, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		files[path] = hash
		manifest.Runtimes[item.key()] = RuntimeEntry{Path: path}
	}
	uiFiles, err := collectUIFiles(uiSource)
	if err != nil {
		return err
	}
	if len(uiFiles) == 0 {
		return fmt.Errorf("%s 目录为空，插件必须包含配置 UI", uiSource)
	}
	for path, hash := range uiFiles {
		files[path] = hash
	}
	if _, ok := files[manifest.UI.Entrypoint]; !ok {
		return fmt.Errorf("ui.entrypoint %q 不存在", manifest.UI.Entrypoint)
	}
	manifest.Files = files
	return nil
}

// collectUIFiles 返回 ui/ 下所有文件的相对路径（使用正斜杠）与 SHA-256。
func collectUIFiles(root string) (map[string]string, error) {
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		result[uiDir+"/"+filepath.ToSlash(relative)] = hash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("扫描 %s: %w", uiDir, err)
	}
	return result, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("读取文件 %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("计算哈希 %s: %w", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func signManifest(manifestBytes []byte, keyPath, keyID string) ([]byte, error) {
	if strings.TrimSpace(keyID) == "" {
		return nil, errors.New("使用 -signing-key 时必须同时提供 -key-id")
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取签名私钥: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("私钥必须是 base64 文本: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("私钥长度必须是 %d 字节，当前为 %d", ed25519.PrivateKeySize, len(decoded))
	}
	signature := ed25519.Sign(ed25519.PrivateKey(decoded), manifestBytes)
	payload, err := json.MarshalIndent(Signature{
		Algorithm: signatureAlg,
		KeyID:     strings.TrimSpace(keyID),
		Signature: base64.StdEncoding.EncodeToString(signature),
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化签名: %w", err)
	}
	return append(payload, '\n'), nil
}

func writePackage(outputPath, distDir, uiSource string, manifestBytes, signatureBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建包文件: %w", err)
	}
	defer func() { _ = file.Close() }()

	archive := zip.NewWriter(file)
	if err := addZipEntry(archive, manifestFile, manifestBytes, 0o644); err != nil {
		return err
	}
	if signatureBytes != nil {
		if err := addZipEntry(archive, signatureFile, signatureBytes, 0o644); err != nil {
			return err
		}
	}
	now := time.Now()
	for _, member := range listArchiveMembers(distDir, uiSource) {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(member.archivePath, runtimesDir+"/") {
			mode = 0o755
		}
		contents, err := os.ReadFile(member.diskPath)
		if err != nil {
			return fmt.Errorf("读取打包文件 %s: %w", member.archivePath, err)
		}
		if err := addZipEntryWithTime(archive, member.archivePath, contents, mode, now); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("写入包文件: %w", err)
	}
	return file.Sync()
}

type archiveMember struct {
	archivePath string
	diskPath    string
}

// listArchiveMembers 列出要打进包的成员：runtimes/ 取自构建产物目录，ui/ 取自 UI 源目录。
func listArchiveMembers(distDir, uiSource string) []archiveMember {
	members := []archiveMember{}
	roots := []struct {
		prefix string
		root   string
	}{
		{runtimesDir, filepath.Join(distDir, runtimesDir)},
		{uiDir, uiSource},
	}
	for _, item := range roots {
		_ = filepath.WalkDir(item.root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil || entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(item.root, path)
			if err != nil {
				return nil
			}
			members = append(members, archiveMember{
				archivePath: item.prefix + "/" + filepath.ToSlash(relative),
				diskPath:    path,
			})
			return nil
		})
	}
	sort.Slice(members, func(left, right int) bool {
		return members[left].archivePath < members[right].archivePath
	})
	return members
}

func addZipEntry(archive *zip.Writer, name string, contents []byte, mode os.FileMode) error {
	return addZipEntryWithTime(archive, name, contents, mode, time.Now())
}

func addZipEntryWithTime(archive *zip.Writer, name string, contents []byte, mode os.FileMode, modified time.Time) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modified}
	header.SetMode(mode)
	writer, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("创建包条目 %s: %w", name, err)
	}
	if _, err := writer.Write(contents); err != nil {
		return fmt.Errorf("写入包条目 %s: %w", name, err)
	}
	return nil
}

// verifyPackage 重新读取生成的包，自检清单可解析、声明文件存在且哈希一致。
func verifyPackage(outputPath string, manifestBytes []byte) error {
	reader, err := zip.OpenReader(outputPath)
	if err != nil {
		return fmt.Errorf("自检打开包: %w", err)
	}
	defer func() { _ = reader.Close() }()

	entries := map[string]*zip.File{}
	for _, file := range reader.File {
		entries[file.Name] = file
	}
	storedManifest, err := readZipEntry(entries[manifestFile])
	if err != nil {
		return fmt.Errorf("自检读取 %s: %w", manifestFile, err)
	}
	if string(storedManifest) != string(manifestBytes) {
		return errors.New("自检失败: 包内清单与生成内容不一致")
	}
	var manifest Manifest
	if err := json.Unmarshal(storedManifest, &manifest); err != nil {
		return fmt.Errorf("自检解析清单: %w", err)
	}
	for path, expected := range manifest.Files {
		file := entries[path]
		if file == nil {
			return fmt.Errorf("自检失败: 缺少已声明文件 %s", path)
		}
		contents, err := readZipEntry(file)
		if err != nil {
			return fmt.Errorf("自检读取 %s: %w", path, err)
		}
		actual := sha256.Sum256(contents)
		if hex.EncodeToString(actual[:]) != expected {
			return fmt.Errorf("自检失败: %s 哈希不匹配", path)
		}
	}
	return nil
}

func readZipEntry(file *zip.File) ([]byte, error) {
	if file == nil {
		return nil, errors.New("条目不存在")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}
