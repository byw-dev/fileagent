import { Tree, Typography } from 'antd'
import type { DataNode } from 'antd/es/tree'

interface DirectoryTreeProps {
  /** Tree data following Ant Design's DataNode structure. */
  treeData: DataNode[]
  /** Callback when a node is selected. */
  onSelect?: (keys: React.Key[]) => void
}

/**
 * DirectoryTree — wraps Ant Design's Tree in directory mode for displaying
 * remote file system paths returned by the list-dir API.
 */
function DirectoryTree({ treeData, onSelect }: DirectoryTreeProps) {
  if (!treeData || treeData.length === 0) {
    return <Typography.Text type="secondary">暂无数据</Typography.Text>
  }

  return (
    <Tree.DirectoryTree
      treeData={treeData}
      onSelect={onSelect}
      defaultExpandAll={false}
    />
  )
}

export default DirectoryTree
