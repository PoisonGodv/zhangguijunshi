package reach

// template.go holds the pure Render function: it selects one of the 8 reach
// templates by Code and fills its title + body from RenderInput, returning
// Rendered or ErrTemplateUnknown for an unknown code. Render touches no Service
// state and no infrastructure, so it is independently callable from tests and
// from (s *Service).Render (which just delegates). All copy is 口语化 and bodies
// stay ≤ 120 runes (tech plan §4.3 reach-copy guidance).

import "strings"

// token map for one render pass. Keys are the {placeholder} tokens, values the
// resolved strings.
type tokens map[string]string

// fill replaces every "{key}" token in tmpl with its value, in token-map order.
func fill(tmpl string, t tokens) string {
	for k, v := range t {
		tmpl = strings.ReplaceAll(tmpl, k, v)
	}
	return tmpl
}

// pick resolves a variable in priority order: explicit field → Vars[key] → def.
// Empty strings at any layer fall through to the next.
func pick(explicit, key string, vars map[string]string, def string) string {
	if explicit != "" {
		return explicit
	}
	if vars != nil {
		if v, ok := vars[key]; ok && v != "" {
			return v
		}
	}
	return def
}

// defaultRecallDays returns the recall window implied by a recall_* code, used
// when the caller did not supply recall_days explicitly.
func defaultRecallDays(code string) string {
	switch code {
	case TplRecall30:
		return "30"
	case TplRecall60:
		return "60"
	case TplRecall90:
		return "90"
	}
	return "30"
}

// Render is the pure template renderer. Unknown code ⇒ ErrTemplateUnknown. Every
// variable falls back to a sensible default so a partially-populated RenderInput
// still yields sendable copy.
func Render(in RenderInput) (Rendered, error) {
	name := pick(in.MemberName, "member_name", in.Vars, "朋友")
	store := pick(in.StoreName, "store_name", in.Vars, "我们店")

	switch in.Code {
	case TplNewWelcome:
		body := fill("{name}你好呀，欢迎加入{store}大家庭！新人专属优惠券已经塞进你的卡包，下次到店直接抵扣，有问题随时问我哦~", tokens{
			"{name}": name, "{store}": store,
		})
		return Rendered{Code: in.Code, Title: "欢迎来到" + store, Body: body}, nil

	case TplBirthday:
		birthday := pick("", "birthday", in.Vars, "今天")
		body := fill("{name}，祝{birthday}生日快乐！{store}为你准备了一份生日礼，当天到店报手机号就能领，祝你度过美好的一天~", tokens{
			"{name}": name, "{birthday}": birthday, "{store}": store,
		})
		return Rendered{Code: in.Code, Title: name + "，生日快乐", Body: body}, nil

	case TplRecall30, TplRecall60, TplRecall90:
		days := pick("", "recall_days", in.Vars, defaultRecallDays(in.Code))
		body := fill("{name}，掐指一算你有{days}天没来{store}啦，怪想你的。特意留了张专属券，这周末来坐坐？手慢可就没了哦~", tokens{
			"{name}": name, "{days}": days, "{store}": store,
		})
		return Rendered{Code: in.Code, Title: name + "，好久不见", Body: body}, nil

	case TplRepurchase:
		product := pick("", "product_name", in.Vars, "好物")
		body := fill("{name}，上次买的{product}快用完了吧？这周正好有活动，回购更划算，店长帮你留了一份，来了直接拿走~", tokens{
			"{name}": name, "{product}": product,
		})
		return Rendered{Code: in.Code, Title: name + "，该补货啦", Body: body}, nil

	case TplBalanceLow:
		balance := pick("", "balance", in.Vars, "少量")
		body := fill("{name}，你卡里余额还剩{balance}元，该充值啦。现在充值有额外赠送，老顾客充得多送得多，需要直接回复我就行~", tokens{
			"{name}": name, "{balance}": balance,
		})
		return Rendered{Code: in.Code, Title: name + "，余额提醒", Body: body}, nil

	case TplPointsExpire:
		points := pick("", "points", in.Vars, "一些")
		body := fill("{name}，你有{points}积分马上要过期啦，别浪费！来店里换个实用小礼物，细则可以问我，清零就太可惜了~", tokens{
			"{name}": name, "{points}": points,
		})
		return Rendered{Code: in.Code, Title: name + "，积分快过期", Body: body}, nil
	}

	return Rendered{}, ErrTemplateUnknown
}
