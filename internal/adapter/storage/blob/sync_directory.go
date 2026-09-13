package blob

// syncDirectoryFn is a narrow test seam for exercising the post-publish
// durability boundary. Production calls always use the platform primitive.
var syncDirectoryFn = syncDirectory
