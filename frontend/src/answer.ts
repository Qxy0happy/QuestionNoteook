// 错题上跟答案图有关的那点判断。
//
// 列表与复习界面都要标出「缺答案的题」，所以判断只写这一处 —— 两边各写一遍
// `q.AnswerHash !== ''`，迟早有一处会忘改。Go 那边的 Question.HasAnswer 是同一个
// 判断（库里约定空串 = 还没拍），这里是给视图用的那一份。
import type { Question } from '../bindings/questionbook/internal/library/models.js';

export function hasAnswer(q: Question | null | undefined): boolean {
  return !!q && q.AnswerHash !== '';
}
