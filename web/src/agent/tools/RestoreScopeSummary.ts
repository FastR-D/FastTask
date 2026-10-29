import { createElement } from 'react'

/** The scope comes from the server's same-batch subtree calculation. */
export function RestoreScopeSummary({ value }: { value: unknown }) {
  if (!value || typeof value !== 'object') return null
  const scope = value as { count?: unknown; tasks?: {task_id?:string;title?:string}[]; truncated?: boolean }
  if (typeof scope.count !== 'number' || scope.count <= 0 || !Array.isArray(scope.tasks)) return null
  return createElement('section', { className: 'agent-proposal-restore-scope', 'aria-label': '本次实际恢复范围' },
    createElement('p', null, `本次实际恢复 ${scope.count} 项，包含所选任务及同一删除批次的子任务：`),
    createElement('ul', null, scope.tasks.map((task, index) => createElement('li', {key:task.task_id??index}, task.title || '未命名任务'))),
    scope.truncated ? createElement('p', null, '这里只展示前 50 项；范围包含上述任务所属的同批子树。') : null,
    createElement('p', null, '其他删除批次保持原样。'))
}
