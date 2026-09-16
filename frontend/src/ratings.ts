// 四档的名字，**只在这里写一遍**。
//
// 复习页拿它做自评那四个按钮，设置页拿它给「四档间隔预览」那一行标名字。抄成两份的话，
// 改一处就会有一处开始说谎 —— 而这两处说的必须是同一件事。
import { Rating } from '../bindings/questionbook/internal/review/models';

// 顺序就是「从差到好」，与 Go 侧 Again/Hard/Good/Easy 的取值一致（1..4）。
// 档位名沿用 CONTEXT.md 里的英文；hint 那句中文是自评时最容易犹豫的地方 ——
// 「Good 还是 Easy」得说清楚。
export const RATINGS = [
  { value: Rating.Again, label: 'Again', hint: '没想起来' },
  { value: Rating.Hard, label: 'Hard', hint: '很费劲' },
  { value: Rating.Good, label: 'Good', hint: '想起来了' },
  { value: Rating.Easy, label: 'Easy', hint: '太简单' },
] as const;

// 界面上拿到的评级是枚举的**数值**（3），不能直接亮出去。
export function ratingLabel(rating: Rating): string {
  return RATINGS.find((r) => r.value === rating)?.label ?? String(rating);
}
