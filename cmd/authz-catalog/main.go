// authz-catalog：授权目录发布。
// 直接复用 customer_and_opportunity/cmd/authz-catalog 模式（internal/platformcatalog）。
// 目录定义见 ../authz/permission-manifest.yaml。
package main

import (
	"log"
	"os"
)

func main() {
	log.Printf("authz-catalog: 占位，manifest 路径=%s", os.Getenv("AUTHZ_MANIFEST_PATH"))
	// TODO: 复用 platformcatalog 包发布目录（PUT /api/v1/applications/{id}/authorization-catalog）
}
