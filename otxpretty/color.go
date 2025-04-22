package otxpretty

import "github.com/fatih/color"

var (
	c_faint = color.New(color.FgBlack, color.Faint)
	c_time  = color.New(color.FgWhite, color.Faint)

	c_debug = color.New(color.FgHiGreen)
	c_info  = color.New(color.FgHiBlue)
	c_warn  = color.New(color.FgHiYellow)
	c_error = color.New(color.FgHiRed)

	c_msg     = color.New(color.FgHiWhite)
	c_ingress = color.RGB(169, 255, 255) // rgb(169, 255, 255)
	c_egress  = color.RGB(255, 194, 169) // rgb(255, 194, 169)

	pastel_colors = []*color.Color{
		color.RGB(255, 179, 186), // rgb(255, 179, 186) - Pastel Pink
		color.RGB(255, 223, 186), // rgb(255, 223, 186) - Pastel Peach
		color.RGB(255, 255, 186), // rgb(255, 255, 186) - Pastel Yellow
		color.RGB(186, 255, 201), // rgb(186, 255, 201) - Pastel Green
		color.RGB(186, 255, 255), // rgb(186, 255, 255) - Pastel Cyan
		color.RGB(186, 201, 255), // rgb(186, 201, 255) - Pastel Blue
		color.RGB(201, 186, 255), // rgb(201, 186, 255) - Pastel Lavender
		color.RGB(255, 186, 255), // rgb(255, 186, 255) - Pastel Magenta
		color.RGB(255, 186, 214), // rgb(255, 186, 214) - Pastel Rose
	}
	dimmed_colors = []*color.Color{
		color.RGB(139, 0, 0),    // rgb(139, 0, 0)     - Dark Red
		color.RGB(80, 0, 0),     // rgb(80, 0, 0)      - Deep Burgundy
		color.RGB(128, 64, 0),   // rgb(128, 64, 0)    - Dark Copper
		color.RGB(204, 85, 0),   // rgb(204, 85, 0)    - Dark Orange
		color.RGB(0, 102, 102),  // rgb(0, 102, 102)   - Dark Cyan
		color.RGB(0, 100, 0),    // rgb(0, 100, 0)     - Dark Green
		color.RGB(85, 107, 47),  // rgb(85, 107, 47)   - Dark Olive Green
		color.RGB(46, 139, 87),  // rgb(46, 139, 87)   - Sea Green
		color.RGB(47, 79, 79),   // rgb(47, 79, 79)    - Dark Slate Gray
		color.RGB(77, 57, 57),   // rgb(77, 57, 57)    - Brownish Gray
		color.RGB(72, 61, 139),  // rgb(72, 61, 139)   - Dark Slate Blue
		color.RGB(72, 61, 178),  // rgb(72, 61, 178)   - Royal Indigo
		color.RGB(58, 95, 205),  // rgb(58, 95, 205)   - Slate Blue
		color.RGB(70, 130, 180), // rgb(70, 130, 180)  - Steel Blue
		color.RGB(45, 82, 160),  // rgb(45, 82, 160)   - Twilight Blue
		color.RGB(47, 79, 117),  // rgb(47, 79, 117)   - Deep Slate Blue
		color.RGB(39, 64, 139),  // rgb(39, 64, 139)   - Dark Cornflower Blue
		color.RGB(0, 51, 102),   // rgb(0, 51, 102)    - Dark Blue
		color.RGB(0, 51, 153),   // rgb(0, 51, 153)    - Deep Cobalt Blue
		color.RGB(25, 25, 112),  // rgb(25, 25, 112)   - Midnight Blue
		color.RGB(128, 0, 128),  // rgb(128, 0, 128)   - Dark Magenta
		color.RGB(75, 0, 130),   // rgb(75, 0, 130)    - Dark Purple
		color.RGB(108, 52, 131), // rgb(108, 52, 131)  - Dark Amethyst
		color.RGB(79, 48, 89),   // rgb(79, 48, 89)    - Grape Purple
		color.RGB(95, 158, 160), // rgb(95, 158, 160)  - Cadet Blue
		color.RGB(54, 69, 79),   // rgb(54, 69, 79)    - Charcoal Gray
		color.RGB(0, 77, 77),    // rgb(0, 77, 77)     - Teal Night
		color.RGB(72, 61, 178),  // rgb(72, 61, 178)   - Royal Indigo
	}
)
