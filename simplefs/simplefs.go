package simplefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/darkweak/storages/core"
	"github.com/dustin/go-humanize"
	"github.com/jellydator/ttlcache/v3"
	"github.com/klauspost/compress/zstd" // 导入 zstd 库
	"github.com/pierrec/lz4/v4"
)

// Simplefs 提供程序类型。
type Simplefs struct {
	cache         *ttlcache.Cache[string, []byte]
	stale         time.Duration // 过期时间
	size          int           // 缓存的最大项目数
	path          string        // 存储目录路径
	logger        core.Logger   // 日志记录器
	actualSize    int64         // 当前缓存的实际大小（字节）
	directorySize int64         // 最大目录大小（字节），-1 表示无限制
	mu            sync.Mutex    // 互斥锁，用于同步访问 actualSize 和 directorySize
	compression   string        // 使用的压缩方法 ("lz4", "zstd", "" 表示不压缩) // 压缩选项
}

// onEvict 是一个回调函数，当缓存中的项目被驱逐时调用。
func onEvict(path string) error {
	return os.Remove(path) // 从文件系统中删除被驱逐的文件
}

// Factory 函数创建新的 Simplefs 实例。
func Factory(simplefsCfg core.CacheProvider, logger core.Logger, stale time.Duration) (core.Storer, error) {
	var directorySize int64

	storagePath := simplefsCfg.Path // 从配置中获取存储路径
	size := 0                       // 默认缓存大小
	directorySize = -1              // 默认目录大小无限制
	compression := ""               // 默认不启用压缩

	simplefsConfiguration := simplefsCfg.Configuration
	if simplefsConfiguration != nil {
		if sfsconfig, ok := simplefsConfiguration.(map[string]interface{}); ok {
			// 大小配置
			if v, found := sfsconfig["size"]; found && v != nil {
				if val, ok := v.(int); ok && val > 0 {
					size = val
				} else if val, ok := v.(float64); ok && val > 0 {
					size = int(val)
				} else if val, ok := v.(string); ok {
					size, _ = strconv.Atoi(val)
				}
			}

			// 路径配置
			if v, found := sfsconfig["path"]; found && v != nil {
				if val, ok := v.(string); ok {
					storagePath = val
				}
			}

			// 目录大小配置
			if v, found := sfsconfig["directory_size"]; found && v != nil {
				if val, ok := v.(int64); ok && val > 0 {
					directorySize = val
				} else if val, ok := v.(float64); ok && val > 0 {
					directorySize = int64(val)
				} else if val, ok := v.(string); ok && val != "" {
					s, _ := humanize.ParseBytes(val)
					//nolint:gosec
					directorySize = int64(s)
				}
			}
			// 压缩方法配置
			if v, found := sfsconfig["compression"]; found && v != nil {
				if val, ok := v.(string); ok {
					compression = strings.ToLower(val) // 将压缩方法转换为小写
				}
			}
		}
	}

	var err error

	// 如果没有配置路径，则回退到当前工作目录
	if storagePath == "" {
		logger.Info("未提供配置路径，回退到当前工作目录。")

		storagePath, err = os.Getwd()
		if err != nil {
			logger.Errorf("无法初始化此工作目录中的存储路径: %#v", err)

			return nil, err
		}
	}

	// 初始化 TTL 缓存
	cache := ttlcache.New(
		//nolint:gosec
		ttlcache.WithCapacity[string, []byte](uint64(size)),
	)

	if cache == nil {
		err = errors.New("无法初始化 simplefs 存储。")
		logger.Error(err)

		return nil, err
	}

	// 创建存储目录，如果不存在
	if err := os.MkdirAll(storagePath, 0o777); err != nil {
		logger.Errorf("无法创建存储目录: %#v", err)

		return nil, err
	}

	logger.Infof("如果需要，已创建存储目录 %s", storagePath)

	go cache.Start() // 启动 TTL 缓存

	// 返回新创建的 Simplefs 实例
	return &Simplefs{
		cache:         cache,
		directorySize: directorySize,
		logger:        logger,
		mu:            sync.Mutex{},
		path:          storagePath,
		size:          size,
		stale:         stale,
		compression:   compression, // 保存压缩选项
	}, nil
}

// Name 返回存储器的名称。
func (provider *Simplefs) Name() string {
	return "SIMPLEFS"
}

// Uuid 返回唯一标识符。
func (provider *Simplefs) Uuid() string {
	return fmt.Sprintf("%s-%d", provider.path, provider.size)
}

// MapKeys 方法返回带有键和值的 map。
func (provider *Simplefs) MapKeys(prefix string) map[string]string {
	keys := map[string]string{}

	// 遍历缓存并收集带有前缀的键
	provider.cache.Range(func(item *ttlcache.Item[string, []byte]) bool {
		if strings.HasPrefix(item.Key(), prefix) {
			k, _ := strings.CutPrefix(item.Key(), prefix)
			keys[k] = string(item.Value()) // 将文件路径存储为值
		}

		return true // 继续迭代
	})

	return keys
}

// ListKeys 方法返回现有键的列表。
func (provider *Simplefs) ListKeys() []string {
	return provider.cache.Keys()
}

// Get 方法返回存储在 Simplefs 中与键对应的响应。
func (provider *Simplefs) Get(key string) []byte {
	result := provider.cache.Get(key) // 从缓存中获取文件路径
	if result == nil {
		provider.logger.Warnf("无法在 Simplefs 中获取键 %s", key)

		return nil // 缓存中未找到键
	}

	filePath := string(result.Value())

	byteValue, err := os.ReadFile(filePath) // 从文件系统读取文件
	if err != nil {
		provider.logger.Errorf("无法从 Simplefs 读取文件 %s: %#v", filePath, err)

		return result.Value() // 如果读取文件失败，则返回文件路径 (回退)
	}

	var decompressedData []byte

	switch provider.compression {
	case "lz4":
		provider.logger.Debugf("尝试使用 lz4 解压缩键 %s", key)
		r := lz4.NewReader(bytes.NewReader(byteValue))
		decompressedData, err = io.ReadAll(r)
		if err != nil {
			provider.logger.Errorf("无法使用 lz4 解压缩键 %s 的数据: %v", key, err)
			return nil // 解压缩失败，返回 nil
		}
	case "zstd":
		provider.logger.Debugf("尝试使用 zstd 解压缩键 %s", key)
		r, err := zstd.NewReader(bytes.NewReader(byteValue))
		if err != nil {
			provider.logger.Errorf("无法创建 zstd 解压缩读取器: %v", err)
			return nil // 解压缩失败，返回 nil
		}
		defer r.Close()
		decompressedData, err = io.ReadAll(r)
		if err != nil {
			provider.logger.Errorf("无法使用 zstd 解压缩键 %s 的数据: %v", key, err)
			return nil // 解压缩失败，返回 nil
		}
	case "": // 未压缩的情况
		provider.logger.Debugf("键 %s 未使用压缩", key)
		decompressedData = byteValue
	default:
		provider.logger.Errorf("不支持的压缩方法: %s", provider.compression)
		return nil // 不支持的压缩方法，返回 nil
	}

	return decompressedData

}

// GetMultiLevel 尝试加载键并检查其中一个链接键是否为 fresh/stale 候选者。
func (provider *Simplefs) GetMultiLevel(key string, req *http.Request, validator *core.Revalidator) (fresh *http.Response, stale *http.Response) {
	// 从缓存中获取映射键
	val := provider.cache.Get(core.MappingKeyPrefix + key)
	if val == nil {
		provider.logger.Errorf("无法在 Simplefs 中获取映射键 %s", core.MappingKeyPrefix+key)

		return fresh, stale // 未找到映射键
	}

	// 基于映射执行 fresh/stale 选举
	fresh, stale, _ = core.MappingElection(provider, val.Value(), req, validator, provider.logger)

	return fresh, stale
}

// recoverEnoughSpaceIfNeeded 在存储新项目之前检查并回收足够的磁盘空间（如果需要）。
func (provider *Simplefs) recoverEnoughSpaceIfNeeded(size int64) {
	// 检查是否强制了目录大小限制，以及存储是否会超出限制
	if provider.directorySize > -1 && provider.actualSize+size > provider.directorySize {
		// 反向迭代缓存（LRU 顺序）
		provider.cache.RangeBackwards(func(item *ttlcache.Item[string, []byte]) bool {
			// 如果没有足够的空间，则删除最旧的项目。
			//nolint:godox
			// TODO: 打开 PR 以公开一个在 LRU 项目上迭代的范围。
			provider.cache.Delete(string(item.Value())) // 从缓存（和物理文件）中删除项目

			return false // 删除一个项目后停止 (可以调整为删除更多项目)
		})

		provider.recoverEnoughSpaceIfNeeded(size) // 在删除项目后递归调用自身
	}
}

// SetMultiLevel 将响应存储到 Simplefs 中，并更新映射键以存储元数据。
func (provider *Simplefs) SetMultiLevel(baseKey, variedKey string, value []byte, variedHeaders http.Header, etag string, duration time.Duration, realKey string) error {
	now := time.Now()

	var compressed bytes.Buffer
	var w *lz4.Writer // 在 if 块外声明压缩写入器

	// 根据压缩选项压缩数据
	switch provider.compression {
	case "zstd":
		zw, err := zstd.NewWriter(&compressed)
		if err != nil {
			provider.logger.Errorf("无法为键 %s 创建 zstd 压缩写入器: %v", variedKey, err)
			return err
		}
		defer zw.Close()
		if _, err = zw.Write(value); err != nil {
			provider.logger.Errorf("无法使用 zstd 压缩键 %s 的数据: %v", variedKey, err)
			return err
		}
	case "lz4", "": // "lz4" 或 不压缩 (默认为 "lz4" 以保持向后兼容)
		w = lz4.NewWriter(&compressed)
		defer w.Close()
		_, err := w.ReadFrom(bytes.NewReader(value))
		if err != nil {
			provider.logger.Errorf("无法使用 lz4 压缩键 %s 的数据: %v", variedKey, err)
			return err
		}
	default:
		provider.logger.Warnf("未知的压缩方法: %s, 不进行压缩存储", provider.compression)
		compressed.Write(value) // 如果方法未知，则不压缩存储
	}

	provider.recoverEnoughSpaceIfNeeded(int64(compressed.Len())) // 如果需要，回收磁盘空间

	joinedFP := filepath.Join(provider.path, url.PathEscape(variedKey)) // 连接目录路径和转义后的键
	//nolint:gosec
	if err := os.WriteFile(joinedFP, compressed.Bytes(), 0o644); err != nil {
		provider.logger.Errorf("无法将文件 %s 写入 Simplefs: %#v", variedKey, err)

		return nil // 写入文件失败
	}

	_ = provider.cache.Set(variedKey, []byte(joinedFP), duration) // 将文件路径存储到缓存中，并设置 TTL

	// 更新映射键
	mappingKey := core.MappingKeyPrefix + baseKey
	item := provider.cache.Get(mappingKey)
	if item == nil {
		provider.logger.Warnf("无法在 Simplefs 中找到映射键 %s", mappingKey)

		item = &ttlcache.Item[string, []byte]{} // 如果未找到映射键，则创建新项目
	}

	// 更新映射元数据
	val, e := core.MappingUpdater(variedKey, item.Value(), provider.logger, now, now.Add(duration), now.Add(duration+provider.stale), variedHeaders, etag, realKey)
	if e != nil {
		return e // 更新映射失败
	}

	provider.logger.Debugf("在 Simplefs 中为键 %s 存储新的映射", variedKey)
	// 用于计算 -(now * 2)
	negativeNow, err := time.ParseDuration(fmt.Sprintf("-%ds", time.Now().Nanosecond()*2))
	if err != nil {
		return fmt.Errorf("无法生成持续时间: %w", err) // 无法生成负持续时间
	}

	_ = provider.cache.Set(mappingKey, val, negativeNow) // 将更新后的映射键存储回缓存，并设置负 TTL (使其立即过期)

	return nil // 成功存储项目和映射键
}

// Set 方法将响应存储在 Simplefs 提供程序中。
func (provider *Simplefs) Set(key string, value []byte, duration time.Duration) error {
	_ = provider.cache.Set(key, value, duration) // 将项目存储到缓存中并设置 TTL

	return nil
}

// Delete 方法将删除 Simplefs 提供程序中与 key 参数对应的响应（如果存在）。
func (provider *Simplefs) Delete(key string) {
	provider.cache.Delete(key) // 从缓存中删除项目
}

// DeleteMany 方法将删除 Simplefs 提供程序中与 regex key 参数对应的多个响应（如果存在）。
func (provider *Simplefs) DeleteMany(key string) {
	rgKey, e := regexp.Compile(key) // 编译正则表达式键
	if e != nil {
		return // 正则表达式无效，忽略
	}

	// 遍历缓存并删除键与正则表达式匹配的项目
	provider.cache.Range(func(item *ttlcache.Item[string, []byte]) bool {
		if rgKey.MatchString(item.Key()) {
			provider.Delete(item.Key()) // 如果键与正则表达式匹配，则删除项目
		}

		return true // 继续迭代
	})
}

// Init 方法将在启动时初始化 Simplefs 提供程序。
func (provider *Simplefs) Init() error {
	// 在每次将项目插入缓存时调用的回调
	provider.cache.OnInsertion(func(_ context.Context, item *ttlcache.Item[string, []byte]) {
		if strings.Contains(item.Key(), core.MappingKeyPrefix) {
			return // 忽略映射键
		}

		// 获取文件信息以计算实际大小
		info, err := os.Stat(string(item.Value()))
		if err != nil {
			provider.logger.Errorf("无法获取文件大小 %s: %#v", item.Key(), err)

			return // 获取文件信息失败
		}

		// 更新实际大小并记录调试日志
		provider.mu.Lock()
		provider.actualSize += info.Size()
		provider.logger.Debugf("实际大小增加: %d, 总计: %d 字节", info.Size(), provider.actualSize)
		provider.mu.Unlock()
	})

	// 在每次从缓存中驱逐项目时调用的回调
	provider.cache.OnEviction(func(_ context.Context, _ ttlcache.EvictionReason, item *ttlcache.Item[string, []byte]) {
		if strings.Contains(string(item.Value()), core.MappingKeyPrefix) {
			return // 忽略映射键
		}
		// 获取文件信息以更新实际大小
		info, err := os.Stat(string(item.Value()))
		if err != nil {
			provider.logger.Errorf("无法获取文件大小 %s: %#v", item.Key(), err)

			return // 获取文件信息失败
		}

		// 更新实际大小并记录调试日志
		provider.mu.Lock()
		provider.actualSize -= info.Size()
		provider.logger.Debugf("实际大小减少: %d, 总计: %d 字节", info.Size(), provider.actualSize)
		provider.mu.Unlock()

		// 调用 onEvict 函数删除物理文件
		if err := onEvict(string(item.Value())); err != nil {
			provider.logger.Errorf("无法删除文件 %s: %#v", item.Key(), err)
		}
	})

	// 从给定目录中的文件重新生成 simplefs 缓存。
	files, _ := os.ReadDir(provider.path)
	provider.logger.Debugf("从给定目录中的文件重新生成 simplefs 缓存。")

	for _, f := range files {
		if !f.IsDir() {
			info, _ := f.Info()
			provider.actualSize += info.Size() // 从现有文件计算实际大小
			provider.logger.Debugf("向实际大小添加 %v 字节，总计 %v 字节。", info.Size(), provider.actualSize)
		}
	}

	return nil // 初始化成功
}

// Reset 方法将重置或关闭提供程序。
func (provider *Simplefs) Reset() error {
	provider.cache.DeleteAll() // 删除缓存中的所有项目
	// TODO: 如果需要，添加从存储目录中删除所有文件的功能

	return nil // 重置成功
}
